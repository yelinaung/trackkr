//go:build linux

package tracker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
)

const (
	swayIdleMarkerIdle   = "trackkr-idle"
	swayIdleMarkerResume = "trackkr-resume"

	// swayIdleRestartDelay paces the supervisor after the child exits.
	// A compositor restart kills swayidle, and retrying immediately
	// would spin until the new session is up.
	swayIdleRestartDelay = 5 * time.Second

	// swayIdleStopGrace bounds both halves of shutdown: how long the
	// child gets between SIGTERM and SIGKILL, and how long Close waits
	// for the supervisor to notice.
	swayIdleStopGrace = 5 * time.Second

	// swayIdleMaxTimeoutSeconds is swayidle's ceiling, not ours. It
	// keeps the timeout in a signed C int of milliseconds and computes
	// seconds * 1000, so anything past this overflows silently into a
	// timeout that is negative or nonsense.
	swayIdleMaxTimeoutSeconds = 2147483
)

// SwayIdleDetector reports idle time by supervising a swayidle child.
//
// Wayland has no equivalent of the X screensaver extension. A client
// cannot ask how long the user has been idle; it can only ask to be
// told when a timeout it picks has elapsed, through
// ext-idle-notify-v1. swayidle already owns that protocol, so the
// detector runs one at the tracker's own threshold and turns its two
// events back into a duration.
type SwayIdleDetector struct {
	timeout time.Duration
	logger  *zerolog.Logger

	// newCmd builds the child. It is a field so tests can substitute a
	// script for swayidle and exercise the supervisor for real.
	newCmd func(ctx context.Context, timeout time.Duration) *exec.Cmd

	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once

	mu        sync.Mutex
	idleSince time.Time
	now       func() time.Time
}

// NewSwayIdleDetector starts the supervisor. It fails when swayidle is
// missing or when the threshold is one swayidle cannot represent, and
// the caller falls back to NopIdleDetector either way.
func NewSwayIdleDetector(threshold time.Duration, logger *zerolog.Logger) (*SwayIdleDetector, error) {
	timeout, err := effectiveTimeout(threshold)
	if err != nil {
		return nil, err
	}

	binary, err := exec.LookPath("swayidle")
	if err != nil {
		return nil, fmt.Errorf("swayidle not found: %w", err)
	}

	if timeout != threshold {
		logger.Info().
			Dur("configured", threshold).
			Dur("effective", timeout).
			Msg("rounded idle threshold up to whole seconds for swayidle")
	}

	return startSwayIdleDetector(timeout, logger,
		func(ctx context.Context, timeout time.Duration) *exec.Cmd {
			return swayIdleCommand(ctx, binary, timeout)
		}), nil
}

// startSwayIdleDetector builds a detector around an already-validated
// timeout and starts its supervisor. Tests use it to run the
// supervisor against a script instead of swayidle.
func startSwayIdleDetector(
	timeout time.Duration,
	logger *zerolog.Logger,
	newCmd func(context.Context, time.Duration) *exec.Cmd,
) *SwayIdleDetector {
	d := &SwayIdleDetector{
		timeout: timeout,
		logger:  logger,
		newCmd:  newCmd,
		done:    make(chan struct{}),
		now:     time.Now,
	}

	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	go d.run(ctx)
	return d
}

// effectiveTimeout rounds a configured threshold to what swayidle can
// take: whole seconds, at least one, at most its own ceiling.
//
// Rounding up rather than to nearest is what makes the tracker's
// idleTime >= cfg.IdleThreshold test hold. IdleTime at the moment of
// the event reports exactly this value, so rounding 1.5s down to 1s
// would run a swayidle whose events the tracker then ignores, and no
// idle period would ever be recorded. Rounding up reports idle
// slightly late, which the poll interval already does.
func effectiveTimeout(threshold time.Duration) (time.Duration, error) {
	if threshold <= 0 {
		return 0, fmt.Errorf("idle threshold must be positive, got %s", threshold)
	}

	// Not the usual (d + time.Second - 1) ceiling: that overflows
	// int64 for a duration near the maximum, which is the very input
	// the bound below exists to catch, wrapping to a small positive
	// number before the bound can look at it.
	seconds := int64(threshold / time.Second)
	if threshold%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	if seconds > swayIdleMaxTimeoutSeconds {
		return 0, fmt.Errorf(
			"idle threshold %s exceeds swayidle's maximum of %ds",
			threshold, swayIdleMaxTimeoutSeconds,
		)
	}
	return time.Duration(seconds) * time.Second, nil
}

// swayIdleCommand builds the child.
//
// -C /dev/null is not optional. swayidle treats config-file events and
// command-line events as cumulative, and a sway user already runs one
// instance from their config -- the one that locks the screen, dims the
// backlight and suspends the machine. A second instance without the
// flag inherits every one of those, so tracking would lock the session
// a second time and suspend the laptop out from under the user.
//
// -w makes swayidle wait for each command before continuing. Without
// it swayidle double-forks each command and moves on, so two unrelated
// processes write the two markers with no ordering between them; an
// idle and a resume close together can then reach the pipe reversed,
// leaving the detector idle for a user who is sitting there typing.
func swayIdleCommand(ctx context.Context, binary string, timeout time.Duration) *exec.Cmd {
	seconds := strconv.FormatInt(int64(timeout/time.Second), 10)

	//nolint:gosec // binary comes from LookPath; arguments are fixed.
	// nosemgrep: gosec.G204-1
	cmd := exec.CommandContext(
		ctx, binary,
		"-w",
		"-C", os.DevNull,
		"timeout", seconds, "echo "+swayIdleMarkerIdle,
		"resume", "echo "+swayIdleMarkerResume,
	)
	// CommandContext kills with SIGKILL by default, which denies
	// swayidle the chance to release its idle notification.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = swayIdleStopGrace
	return cmd
}

// IdleTime reports how long the user has been idle, or zero.
func (d *SwayIdleDetector) IdleTime(_ context.Context) (time.Duration, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.idleSince.IsZero() {
		return 0, nil
	}
	elapsed := d.now().Sub(d.idleSince)
	if elapsed < 0 {
		return 0, nil
	}
	return elapsed, nil
}

// Close stops the child and waits for the supervisor to finish.
func (d *SwayIdleDetector) Close() {
	d.closeOnce.Do(func() {
		d.cancel()
		select {
		case <-d.done:
		case <-time.After(swayIdleStopGrace):
			d.logger.Warn().Msg("timed out waiting for swayidle to stop")
		}
		d.markActive()
	})
}

// run keeps one swayidle alive for as long as the detector is open.
func (d *SwayIdleDetector) run(ctx context.Context) {
	defer close(d.done)

	for ctx.Err() == nil {
		err := d.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}

		// The resume marker that would have cleared this died with the
		// process, so a restart must not carry idle state across it.
		d.markActive()
		d.logger.Warn().Err(err).Msg("swayidle exited, restarting")

		select {
		case <-ctx.Done():
			return
		case <-time.After(swayIdleRestartDelay):
		}
	}
}

func (d *SwayIdleDetector) runOnce(ctx context.Context) error {
	cmd := d.newCmd(ctx, d.timeout)
	d.applyDisplay(ctx, cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("piping swayidle stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting swayidle: %w", err)
	}

	d.consume(stdout)

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("swayidle exited: %w", err)
	}
	return nil
}

// applyDisplay points the child at the display that exists now.
//
// swayidle connects to WAYLAND_DISPLAY, and a restarted compositor may
// pick a different name. Without this, every replacement child would
// dial a socket that is gone and exit immediately -- a restart loop
// that never recovers and reports no idle for the rest of the session.
// When nothing resolves, the inherited value stands, which is right
// whenever the display name was reused.
func (d *SwayIdleDetector) applyDisplay(ctx context.Context, cmd *exec.Cmd) {
	display, err := discoverWaylandDisplay(ctx)
	if err != nil {
		d.logger.Debug().Err(err).Msg("keeping the inherited WAYLAND_DISPLAY")
		return
	}
	cmd.Env = replaceEnv(cmd.Environ(), "WAYLAND_DISPLAY", display)
}

// replaceEnv sets key to value, dropping any existing entry rather
// than appending a second one: a duplicate leaves which value the
// child sees up to its libc.
func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}

// consume reads markers until the stream ends. A read error ends the
// stream like any other close: the supervisor is already waiting to
// restart the child, so there is nothing else to do with it.
func (d *SwayIdleDetector) consume(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		switch strings.TrimSpace(scanner.Text()) {
		case swayIdleMarkerIdle:
			d.markIdle()
		case swayIdleMarkerResume:
			d.markActive()
		}
	}
	if err := scanner.Err(); err != nil {
		d.logger.Debug().Err(err).Msg("swayidle output ended with an error")
	}
}

// markIdle backdates to when the user actually stopped. swayidle fires
// the timeout after the threshold has already passed, so subtracting
// it is what lets the tracker's endedAt land on the last moment of
// activity rather than on the notification.
func (d *SwayIdleDetector) markIdle() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.idleSince = d.now().Add(-d.timeout)
}

func (d *SwayIdleDetector) markActive() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.idleSince = time.Time{}
}
