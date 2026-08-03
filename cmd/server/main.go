package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/db"
	"github.com/yelinaung/trackkr/internal/server"
	"golang.org/x/crypto/bcrypt"
)

const (
	serverBinary           = "trackkr-server"
	commandServe           = "serve"
	commandMigrate         = "migrate"
	commandCreateUser      = "create-user"
	commandCreateDevice    = "create-device"
	commandMigrationStatus = "migration-status"
	commandMigrationForce  = "migration-force"
	commandVersion         = "version"
)

var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).
		With().Timestamp().Logger()

	command, err := commandFor(os.Args)
	if err != nil {
		logger.Fatal().Err(err).
			Msg("usage: trackkr-server [serve|migrate|migration-status|migration-force VERSION|create-user|create-device|version]")
	}
	if command == commandVersion {
		fmt.Println(versionReport())
		return
	}

	configPath := "config.toml"
	if v := os.Getenv("TRACKKR_CONFIG"); v != "" {
		configPath = v
	}

	cfg, err := server.LoadConfigOrDefault(configPath)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to load config")
	}
	dsn, err := cfg.Database.DSN()
	if err != nil {
		logger.Fatal().Err(err).Msg("invalid database configuration")
	}

	ctx := context.Background()
	switch command {
	case commandMigrate:
		logger.Info().Msg("running database migrations")
		if err := db.RunMigrations(dsn); err != nil {
			logger.Fatal().Err(err).Msg("failed to run migrations")
		}
	case commandMigrationStatus:
		state, err := db.GetMigrationStatus(dsn)
		if err != nil {
			logger.Fatal().Err(err).Msg("failed to inspect migration status")
		}
		fmt.Printf("version=%d dirty=%t applied=%t\n", state.Version, state.Dirty, state.Applied)
	case commandMigrationForce:
		version, err := strconv.Atoi(os.Args[2])
		if err != nil {
			logger.Fatal().Err(err).Msg("migration version must be an integer")
		}
		if err := db.ForceMigrationVersion(dsn, version); err != nil {
			logger.Fatal().Err(err).Msg("failed to force migration version")
		}
		logger.Warn().Int("version", version).Msg("migration version forced")
	case commandCreateUser, commandCreateDevice:
		pool, err := db.NewPool(ctx, dsn)
		if err != nil {
			logger.Fatal().Err(err).Msg("failed to connect to database")
		}
		defer pool.Close()

		queries := db.NewQueries(pool)
		switch command {
		case commandCreateUser:
			runCreateUser(ctx, queries, &logger)
		case commandCreateDevice:
			runCreateDevice(ctx, queries, &logger)
		}
	case commandServe:
		pool, err := db.NewPool(ctx, dsn)
		if err != nil {
			logger.Fatal().Err(err).Msg("failed to connect to database")
		}
		defer pool.Close()

		runServe(ctx, cfg, pool, &logger)
	}
}

func commandFor(args []string) (string, error) {
	if len(args) < 2 {
		return commandServe, nil
	}

	command := args[1]
	switch command {
	case commandServe, commandMigrate, commandMigrationStatus, commandVersion:
		if len(args) != 2 {
			return "", fmt.Errorf("%s does not accept arguments", command)
		}
		return command, nil
	case commandMigrationForce:
		if len(args) != 3 {
			return "", fmt.Errorf("%s requires a version", command)
		}
		return command, nil
	case commandCreateUser, commandCreateDevice:
		return command, nil
	default:
		return "", fmt.Errorf("unknown command %q", command)
	}
}

func versionReport() string {
	return fmt.Sprintf("version=%s commit=%s build_date=%s", version, commit, buildDate)
}

func runServe(
	ctx context.Context,
	cfg *server.Config,
	pool *pgxpool.Pool,
	logger *zerolog.Logger,
) {
	srv, err := server.New(cfg, pool, logger)
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
	if err := serveUntilShutdown(shutdownCtx, httpServer, logger); err != nil {
		logger.Fatal().Err(err).Msg("server error")
	}
}

type gracefulHTTPServer interface {
	ListenAndServe() error
	Shutdown(context.Context) error
	Close() error
}

const serverShutdownTimeout = 15 * time.Second

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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
		shutdownErr := httpServer.Shutdown(shutdownCtx)
		cancel()
		var resultErr error
		if shutdownErr != nil {
			if errors.Is(shutdownErr, context.DeadlineExceeded) {
				logger.Warn().Dur("timeout", serverShutdownTimeout).Msg("forcing HTTP server shutdown")
			} else {
				resultErr = fmt.Errorf("shutting down HTTP server: %w", shutdownErr)
			}
			if err := httpServer.Close(); err != nil {
				return fmt.Errorf("force-closing HTTP server: %w", err)
			}
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return resultErr
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
