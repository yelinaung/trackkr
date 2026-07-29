package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/db"
	"github.com/yelinaung/trackkr/internal/server"
	"golang.org/x/crypto/bcrypt"
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

	queries := db.NewQueries(pool)

	// Handle subcommands
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "create-user":
			runCreateUser(ctx, queries, &logger)
			return
		case "create-device":
			runCreateDevice(ctx, queries, &logger)
			return
		default:
			logger.Fatal().
				Str("command", os.Args[1]).
				Msg("unknown command; usage: trackkr-server [create-user|create-device]")
		}
	}

	srv, err := server.New(cfg, pool, &logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to start server")
	}
	defer srv.Close()

	httpServer := &http.Server{
		Addr:              cfg.Server.Addr(),
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	shutdownCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info().Str("addr", cfg.Server.Addr()).Msg("starting server")
	if err := serveUntilShutdown(shutdownCtx, httpServer, &logger); err != nil {
		logger.Fatal().Err(err).Msg("server error")
	}
}

type gracefulHTTPServer interface {
	ListenAndServe() error
	Shutdown(context.Context) error
}

func serveUntilShutdown(
	ctx context.Context,
	httpServer gracefulHTTPServer,
	logger *zerolog.Logger,
) error {
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.Info().Msg("shutting down")
		if err := httpServer.Shutdown(context.Background()); err != nil {
			return fmt.Errorf("shutting down HTTP server: %w", err)
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// runCreateDevice registers a device and prints its API key.
//
// The key goes to stdout on its own line and everything else to the
// logger, so a script can capture it without parsing log output. That
// is the whole reason this exists: without it, automating a dev setup
// means inserting rows into the database by hand.
func runCreateDevice(ctx context.Context, queries *db.Queries, logger *zerolog.Logger) {
	if len(os.Args) < 4 {
		fmt.Fprintf(os.Stderr, "usage: trackkr-server create-device <username> <device-name> [type]\n")
		os.Exit(1)
	}

	username := os.Args[2]
	name := os.Args[3]
	deviceType := "desktop"
	if len(os.Args) > 4 {
		deviceType = os.Args[4]
	}

	user, err := queries.GetUserByUsername(ctx, username)
	if err != nil {
		logger.Fatal().Err(err).Str("username", username).Msg("no such user")
	}

	// Reuse a device rather than adding another. Re-running a setup
	// script should be idempotent; otherwise the device filter fills
	// with identically named entries and only the newest has activity.
	//
	// Both name and type must match, since the type is a parameter and
	// reusing across types would silently ignore it. ListDevicesByUser
	// returns oldest first, so the scan runs backwards: where historical
	// duplicates exist, the newest is the one already reporting.
	existing, err := queries.ListDevicesByUser(ctx, user.ID)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to list devices")
	}
	for _, device := range slices.Backward(existing) {
		if device.Name != name || device.DeviceType != deviceType {
			continue
		}
		logger.Info().
			Int64("id", device.ID).
			Str("name", name).
			Str("type", deviceType).
			Msg("device already exists, reusing it")
		fmt.Println(device.APIKey)
		return
	}

	apiKey, err := server.GenerateAPIKey()
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to generate api key")
	}

	device, err := queries.CreateDevice(ctx, user.ID, name, deviceType, apiKey)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create device")
	}

	logger.Info().
		Int64("id", device.ID).
		Str("name", device.Name).
		Msg("device created")

	fmt.Println(device.APIKey)
}

func runCreateUser(ctx context.Context, queries *db.Queries, logger *zerolog.Logger) {
	if len(os.Args) < 4 {
		fmt.Fprintf(os.Stderr, "usage: trackkr-server create-user <username> <password>\n")
		os.Exit(1)
	}

	username := os.Args[2]
	password := os.Args[3]

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to hash password")
	}

	user, err := queries.CreateUser(ctx, username, string(hash))
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create user")
	}

	logger.Info().
		Int64("id", user.ID).
		Str("username", user.Username).
		Msg("user created")
}
