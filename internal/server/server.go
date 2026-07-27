package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/db"
	"github.com/yelinaung/trackkr/web"
)

const (
	loginAttemptLimit  = 10
	loginAttemptWindow = 15 * time.Minute
)

// APIQuerier is what the device-facing API needs. It stays separate from
// WebQuerier so a fake for one does not have to implement the other.
type APIQuerier interface {
	GetDeviceByAPIKey(ctx context.Context, apiKey string) (*db.DeviceRow, error)
	InsertActivityRecords(ctx context.Context, records []db.ActivityRecordRow) (int, error)
	ListDevicesByUser(ctx context.Context, userID int64) ([]db.DeviceRow, error)
}

type Server struct {
	config *Config
	router *chi.Mux
	logger *zerolog.Logger

	// Narrow dependencies rather than one wide interface, so a test
	// populates only the fields whose routes it exercises.
	api      APIQuerier
	sessions SessionQuerier
	web      WebQuerier

	templates *templates
	codec     *sessionCodec
	limiter   *attemptLimiter
	loc       *time.Location
}

// New builds the server. It is fallible because two things can only be
// checked at startup: the config, since a weak session secret must not
// boot, and the templates, since a parse error must not wait for a
// request to surface.
func New(cfg *Config, pool *pgxpool.Pool, logger *zerolog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	loc, err := cfg.Server.Location()
	if err != nil {
		return nil, err
	}

	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}

	queries := db.NewQueries(pool)
	s := &Server{
		config:    cfg,
		logger:    logger,
		api:       queries,
		sessions:  queries,
		web:       queries,
		templates: tmpl,
		codec:     newSessionCodec(cfg.Auth.SessionSecret, cfg.Server.SecureCookies),
		limiter:   newAttemptLimiter(loginAttemptLimit, loginAttemptWindow),
		loc:       loc,
	}
	s.router = newRouter(s)
	return s, nil
}

func newRouter(s *Server) *chi.Mux {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(SecurityHeaders)

	// API routes (API key auth — ingest and device listing)
	r.Route("/api/v1", func(api chi.Router) {
		api.Use(APIKeyAuth(s.api))
		api.Post("/activity", HandleIngestActivity(s.api))
		api.Post("/heartbeat", HandleHeartbeat())
		api.Get("/devices", HandleListDevices(s.api))
	})

	h := &webHandlers{
		queries:   s.web,
		templates: s.templates,
		codec:     s.codec,
		limiter:   s.limiter,
		loc:       s.loc,
		logger:    s.logger,
		allowReg:  s.config.Auth.AllowRegistration,
	}

	// Static assets are public: gating them would load the login page
	// with no CSS, and style-src 'self' leaves no CDN fallback.
	r.Handle("/static/*", http.FileServer(http.FS(web.Static)))

	r.Group(func(pub chi.Router) {
		pub.Use(RequireCSRF(s.codec))
		pub.Get("/login", h.handleLoginForm())
		pub.Post("/login", h.handleLogin())
		// Logout stays public: clearing a cookie for an already-expired
		// session should reach the login page, not a 401.
		pub.Post("/logout", h.handleLogout())

		if s.config.Auth.AllowRegistration {
			pub.Get("/register", h.handleRegisterForm())
			pub.Post("/register", h.handleRegister())
		}
	})

	r.Group(func(priv chi.Router) {
		priv.Use(RequireSession(s.codec, s.sessions))
		priv.Use(RequireCSRF(s.codec))
		priv.Get("/", h.handleDashboard())
		priv.Get("/timeline", h.handleTimeline())
		priv.Get("/devices", h.handleDevices())
		priv.Post("/devices", h.handleCreateDevice())
		priv.Delete("/devices/{id}", h.handleDeleteDevice())
	})

	return r
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}
