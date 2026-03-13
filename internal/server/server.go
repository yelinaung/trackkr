package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/db"
)

type Server struct {
	config  *Config
	router  *chi.Mux
	queries *db.Queries
	logger  zerolog.Logger
}

func New(cfg *Config, pool *pgxpool.Pool, logger zerolog.Logger) *Server {
	queries := db.NewQueries(pool)

	s := &Server{
		config:  cfg,
		router:  chi.NewRouter(),
		queries: queries,
		logger:  logger,
	}

	s.setupRoutes()
	return s
}

func (s *Server) setupRoutes() {
	s.router.Use(middleware.RequestID)
	s.router.Use(middleware.RealIP)
	s.router.Use(middleware.Recoverer)

	// API routes (API key auth — ingest only, no device management)
	s.router.Route("/api/v1", func(r chi.Router) {
		r.Use(APIKeyAuth(s.queries))
		r.Post("/activity", HandleIngestActivity(s.queries))
		r.Post("/heartbeat", HandleHeartbeat())
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}
