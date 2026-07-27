package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/tracker"
)

const httpTimeout = 30 * time.Second

func main() {
	configPath := flag.String("config", tracker.DefaultConfigPath(), "path to config file")
	debug := flag.Bool("debug", false, "enable debug logging")
	flag.Parse()

	level := zerolog.InfoLevel
	if *debug {
		level = zerolog.DebugLevel
	}
	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).
		Level(level).
		With().Timestamp().Logger()

	if err := run(&logger, *configPath); err != nil {
		logger.Fatal().Err(err).Msg("trackkrd failed")
	}

	logger.Info().Msg("trackkrd stopped")
}

func run(logger *zerolog.Logger, configPath string) error {
	cfg, err := tracker.LoadConfigOrDefault(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if cfg.APIKey == "" {
		return fmt.Errorf(
			"no api_key configured; set api_key in %s or TRACKKR_API_KEY",
			configPath,
		)
	}

	window, err := tracker.NewWindowDetector()
	if err != nil {
		return fmt.Errorf("window detection unavailable: %w", err)
	}
	idle := tracker.NewIdleDetectorOrNop(logger)

	client := &http.Client{Timeout: httpTimeout}
	reporter := tracker.NewReporter(cfg, client, logger)
	trk := tracker.NewTracker(cfg, window, idle, reporter, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Go(func() { reporter.Run(ctx) })

	logger.Info().
		Str("server", cfg.ServerURL).
		Str("device", cfg.DeviceName).
		Dur("poll_interval", cfg.PollInterval.Duration).
		Dur("idle_threshold", cfg.IdleThreshold.Duration).
		Msg("starting trackkrd")

	// Run blocks until ctx is cancelled, finalizing the in-flight
	// record on the way out.
	trackErr := trk.Run(ctx)

	// The reporter goroutine also stops on ctx.Done; wait for it
	// before the final flush so both do not send at once.
	wg.Wait()

	return errors.Join(trackErr, reporter.Shutdown())
}
