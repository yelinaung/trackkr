package server

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/db"
	"github.com/yelinaung/trackkr/internal/favicon"
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

type databasePinger interface {
	Ping(context.Context) error
}

type Server struct {
	config *Config
	router *chi.Mux
	logger *zerolog.Logger

	trustedProxies []netip.Prefix

	// Narrow dependencies rather than one wide interface, so a test
	// populates only the fields whose routes it exercises.
	api              APIQuerier
	database         databasePinger
	sessions         SessionQuerier
	web              WebQuerier
	iconRead         appIconReader
	iconWrite        appIconWriter
	siteIcons        siteIconStore
	siteRefresh      siteIconRefreshQueue
	closeSiteRefresh func()

	templates *templates
	codec     *sessionCodec
	limiter   *attemptLimiter
	iconLimit *slidingWindowLimiter
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
	trustedProxies, err := cfg.Server.TrustedProxies()
	if err != nil {
		return nil, err
	}

	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}

	queries := db.NewQueries(pool)
	refresher := newSiteIconRefresher(
		queries,
		favicon.NewFetcher(),
		logger,
		defaultSiteIconRefresherConfig(),
	)
	s := &Server{
		config:           cfg,
		logger:           logger,
		trustedProxies:   trustedProxies,
		api:              queries,
		database:         pool,
		sessions:         queries,
		web:              queries,
		iconRead:         queries,
		iconWrite:        queries,
		siteIcons:        queries,
		siteRefresh:      refresher,
		closeSiteRefresh: refresher.Close,
		templates:        tmpl,
		codec:            newSessionCodec(cfg.Auth.SessionSecret, cfg.Server.SecureCookies),
		limiter:          newAttemptLimiter(loginAttemptLimit, loginAttemptWindow),
		iconLimit:        newSlidingWindowLimiter(appIconRateLimit, appIconRateWindow),
		loc:              loc,
	}
	s.router = newRouter(s)
	return s, nil
}

func newRouter(s *Server) *chi.Mux {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(SecurityHeaders(s.config.Server.SecureCookies))
	r.Get("/healthz", handleHealth)
	r.Get("/readyz", s.handleReady)

	// API routes (API key auth — ingest and device listing)
	r.Route("/api/v1", func(api chi.Router) {
		api.Use(APIKeyAuth(s.api))
		api.Post("/activity", HandleIngestActivity(s.api))
		api.Post("/app-icons", handleAppIconUpload(s.iconWrite, s.iconLimit, s.logger))
		api.Post("/heartbeat", HandleHeartbeat())
		api.Get("/devices", HandleListDevices(s.api))
	})

	h := &webHandlers{
		queries:        s.web,
		icons:          s.iconRead,
		siteIcons:      s.siteIcons,
		siteRefresh:    s.siteRefresh,
		templates:      s.templates,
		codec:          s.codec,
		limiter:        s.limiter,
		loc:            s.loc,
		logger:         s.logger,
		allowReg:       s.config.Auth.AllowRegistration,
		trustedProxies: s.trustedProxies,
	}

	// Static assets are public: gating them would load the login page
	// with no CSS, and style-src 'self' leaves no CDN fallback.
	r.Handle("/static/*", noDirListing(http.FileServer(http.FS(web.Static))))

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
		priv.Get("/activity", h.handleActivityDetail())
		priv.Get("/activity/panel", h.handleActivityPanel())
		priv.Get("/app-icons/{id}/{sha256}.png", h.handleAppIcon())
		priv.Get("/site-icons/{site}", h.handleSiteIcon())
		priv.Get("/devices", h.handleDevices())
		priv.Post("/devices", h.handleCreateDevice())
		priv.Delete("/devices/{id}", h.handleDeleteDevice())
		priv.Get("/categories", h.handleCategories())
		priv.Post("/categories", h.handleCreateCategory())
		priv.Post("/categories/{id}", h.handleUpdateCategory())
		priv.Delete("/categories/{id}", h.handleDeleteCategory())
		priv.Post("/categories/assignments", h.handleSetAppCategory())
		priv.Post("/activity/records/{id}/category", h.handleSetRecordCategory())
	})

	return r
}

// noDirListing stops http.FileServer from rendering a browsable index of
// the embedded assets.
func noDirListing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// Close stops background favicon work and waits for cancellable requests.
func (s *Server) Close() {
	if s.closeSiteRefresh != nil {
		s.closeSiteRefresh()
	}
}
