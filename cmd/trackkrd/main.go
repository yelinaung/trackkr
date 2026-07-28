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
	printToken := flag.Bool("print-extension-token", false,
		"print a new browser-extension token and exit")
	flag.Parse()

	// Generation is its own command because the daemon refuses to start
	// with the listener enabled and no token: a daemon that will not
	// start could never generate one.
	if *printToken {
		token, err := tracker.GenerateExtensionToken()
		if err != nil {
			fmt.Fprintf(os.Stderr, "generating token: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(token)
		return
	}

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

	// Window detection and the browser listener are independent sources.
	// On a platform without a window detector the extension can still
	// report tabs, so a missing detector is only fatal when it is the
	// daemon's sole reason to exist.
	window, err := d.newWindow()
	if err != nil {
		if !cfg.ExtensionEnabled {
			return fmt.Errorf("window detection unavailable: %w", err)
		}
		logger.Warn().Err(err).
			Msg("window detection unavailable; reporting browser activity only")
	}

	client := &http.Client{Timeout: httpTimeout}
	reporter := tracker.NewReporter(cfg, client, logger)

	var trk *tracker.Tracker
	if window != nil {
		trk = tracker.NewTracker(cfg, window, d.newIdle(logger), reporter, logger)
	}

	// Own a child context so the reporter goroutine can be released
	// as soon as the tracker returns, whatever cancelled the parent.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Go(func() { reporter.Run(ctx) })

	// A listener that cannot bind is a failure, not a warning: in
	// extension-only mode the daemon would otherwise sit on ctx.Done
	// capturing nothing, and alongside a window detector it would run
	// quietly without the feature the config asked for.
	listenerErr := make(chan error, 1)
	if cfg.ExtensionEnabled {
		ext := tracker.NewExtensionServer(cfg, reporter, logger)
		wg.Go(func() {
			if err := ext.Run(ctx); err != nil {
				listenerErr <- err
				cancel()
			}
		})
	}

	logger.Info().
		Str("server", cfg.ServerURL).
		Dur("poll_interval", cfg.PollInterval.Duration).
		Dur("idle_threshold", cfg.IdleThreshold.Duration).
		Msg("starting trackkrd")

	// Run blocks until ctx is cancelled, finalizing the in-flight
	// record on the way out. With no window detector there is nothing to
	// poll, so wait on the context directly and let the listener work.
	if trk != nil {
		trk.Run(ctx)
	} else {
		<-ctx.Done()
	}
	cancel()

	// Wait for the reporter goroutine before the final flush so both
	// do not send at once.
	wg.Wait()

	var extErr error
	select {
	case extErr = <-listenerErr:
	default:
	}

	return errors.Join(extErr, reporter.Shutdown())
}
