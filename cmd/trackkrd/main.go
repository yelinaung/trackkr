package main

import (
	"context"
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

// detectors supplies the platform-specific pieces of the daemon so
// tests can drive a full lifecycle without X11.
type detectors struct {
	newWindow func() (tracker.WindowDetector, error)
	newIdle   func(*zerolog.Logger) tracker.IdleDetector
}

func platformDetectors() detectors {
	return detectors{
		newWindow: tracker.NewWindowDetector,
		newIdle:   tracker.NewIdleDetectorOrNop,
	}
}

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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, &logger, *configPath, platformDetectors()); err != nil {
		logger.Fatal().Err(err).Msg("trackkrd failed")
	}

	logger.Info().Msg("trackkrd stopped")
}

func run(
	ctx context.Context,
	logger *zerolog.Logger,
	configPath string,
	d detectors,
) error {
	cfg, err := tracker.LoadConfigOrDefault(configPath)
	if err != nil {
		return err
	}
	if cfg.APIKey == "" {
		return fmt.Errorf(
			"no api_key configured; set api_key in %s or TRACKKR_API_KEY",
			configPath,
		)
	}

	window, err := d.newWindow()
	if err != nil {
		return fmt.Errorf("window detection unavailable: %w", err)
	}
	idle := d.newIdle(logger)

	client := &http.Client{Timeout: httpTimeout}
	reporter := tracker.NewReporter(cfg, client, logger)
	trk := tracker.NewTracker(cfg, window, idle, reporter, logger)

	// Own a child context so the reporter goroutine can be released
	// as soon as the tracker returns, whatever cancelled the parent.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Go(func() { reporter.Run(ctx) })

	logger.Info().
		Str("server", cfg.ServerURL).
		Dur("poll_interval", cfg.PollInterval.Duration).
		Dur("idle_threshold", cfg.IdleThreshold.Duration).
		Msg("starting trackkrd")

	// Run blocks until ctx is cancelled, finalizing the in-flight
	// record on the way out.
	trk.Run(ctx)
	cancel()

	// Wait for the reporter goroutine before the final flush so both
	// do not send at once.
	wg.Wait()

	return reporter.Shutdown()
}
