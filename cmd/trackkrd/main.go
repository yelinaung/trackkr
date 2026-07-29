package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
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
	newWindow func(*tracker.Config, *zerolog.Logger) (tracker.WindowDetector, error)
	newIdle   func(*tracker.Config, *zerolog.Logger) tracker.IdleDetector
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
	window, err := d.newWindow(cfg, logger)
	if err != nil {
		if !cfg.ExtensionEnabled {
			return fmt.Errorf("window detection unavailable: %w", err)
		}
		logger.Warn().Err(err).
			Msg("window detection unavailable; reporting browser activity only")
	}

	// Bind before constructing the reporter, not after. A squatted port
	// must fail startup the way invalid config does, and NewReporter
	// loads pending.json and deletes it -- so returning an error after
	// that point would throw away records that were safe on disk.
	var extListener net.Listener
	if cfg.ExtensionEnabled {
		extListener, err = tracker.ListenExtension(ctx, cfg.ExtensionAddr)
		if err != nil {
			return err
		}
		defer func() { _ = extListener.Close() }()
	}

	client := &http.Client{Timeout: httpTimeout}
	reporter := tracker.NewReporter(cfg, client, logger)

	var trk *tracker.Tracker
	if window != nil {
		trk = tracker.NewTracker(cfg, window, d.newIdle(cfg, logger), reporter, logger)
	}

	// Own a child context so the reporter goroutine can be released
	// as soon as the tracker returns, whatever cancelled the parent.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Go(func() { reporter.Run(ctx) })

	// The bind already succeeded, so a failure here is a serving fault:
	// still fatal, since the daemon would otherwise run without the
	// feature the config asked for.
	listenerErr := make(chan error, 1)
	if extListener != nil {
		ext := tracker.NewExtensionServer(cfg, extListener, reporter, logger)
		wg.Go(func() {
			if err := ext.Serve(ctx); err != nil {
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
