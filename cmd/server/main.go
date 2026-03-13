package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/db"
	"github.com/yelinaung/trackkr/internal/server"
)

func main() {
	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).
		With().Timestamp().Logger()

	configPath := "config.toml"
	if v := os.Getenv("TRACKKR_CONFIG"); v != "" {
		configPath = v
	}

	cfg, err := server.LoadConfig(configPath)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to load config")
	}

	// Run database migrations
	logger.Info().Msg("running database migrations")
	if err := db.RunMigrations(cfg.Database.DSN()); err != nil {
		logger.Fatal().Err(err).Msg("failed to run migrations")
	}

	// Connect to database
	ctx := context.Background()
	pool, err := db.NewPool(ctx, cfg.Database.DSN())
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to connect to database")
	}
	defer pool.Close()

	srv := server.New(cfg, pool, logger)

	httpServer := &http.Server{
		Addr:    cfg.Server.Addr(),
		Handler: srv,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		logger.Info().Str("signal", sig.String()).Msg("shutting down")
		httpServer.Shutdown(context.Background())
	}()

	logger.Info().Str("addr", cfg.Server.Addr()).Msg("starting server")
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatal().Err(err).Msg("server error")
	}
}
