package server

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/db"
)

// Querier defines the query methods used by HTTP handlers and middleware.
type Querier interface {
	GetDeviceByAPIKey(ctx context.Context, apiKey string) (*db.DeviceRow, error)
	InsertActivityRecords(ctx context.Context, records []db.ActivityRecordRow) (int, error)
}

type Server struct {
	config  *Config
	router  *chi.Mux
	queries Querier
	logger  *zerolog.Logger
}

func New(cfg *Config, pool *pgxpool.Pool, logger *zerolog.Logger) *Server {
	queries := db.NewQueries(pool)

	s := &Server{
		config:  cfg,
		queries: queries,
		logger:  logger,
	}
	s.router = newRouter(s)
	return s
}

func newRouter(s *Server) *chi.Mux {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	// API routes (API key auth — ingest only, no device management)
	r.Route("/api/v1", func(api chi.Router) {
		api.Use(APIKeyAuth(s.queries))
		api.Post("/activity", HandleIngestActivity(s.queries))
		api.Post("/heartbeat", HandleHeartbeat())
	})
	return r
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}
