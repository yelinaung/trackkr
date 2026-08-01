//go:build linux

package tracker

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// XIdleDetector uses xprintidle to detect user idle time on X11.
type XIdleDetector struct {
	xprintidlePath string
}

// NewXIdleDetector creates an idle detector, verifying that
// xprintidle is installed.
func NewXIdleDetector() (*XIdleDetector, error) {
	path, err := exec.LookPath("xprintidle")
	if err != nil {
		return nil, fmt.Errorf("xprintidle not found: %w", err)
	}
	return &XIdleDetector{xprintidlePath: path}, nil
}

// IdleTime returns the current user idle duration.
func (x *XIdleDetector) IdleTime(ctx context.Context) (time.Duration, error) {
	out, err := exec.CommandContext(ctx, x.xprintidlePath).Output() //nolint:gosec // nosemgrep // gitlab-advanced-sast-exclude -- path validated by LookPath
	if err != nil {
		return 0, fmt.Errorf("running xprintidle: %w", err)
	}
	return parseIdleMs(string(out))
}

// parseIdleMs parses xprintidle output (milliseconds as a string)
// into a time.Duration.
func parseIdleMs(output string) (time.Duration, error) {
	ms, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing idle ms %q: %w", output, err)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// NewIdleDetectorOrNop returns the session's idle detector: swayidle
// on Wayland, xprintidle on X11, and a NopIdleDetector when neither is
// available.
//
// A Wayland session never falls back to xprintidle. It runs happily
// under XWayland and returns a plausible number, but XWayland only
// counts the events it receives itself, so the counter climbs while
// the user types in a native Wayland application. The tracker would
// cross its threshold and stop recording a session that is very much
// active. Nop reports zero instead: over-recording an unattended
// session is recoverable, silently dropping an active one is not.
func NewIdleDetectorOrNop(cfg *Config, logger *zerolog.Logger) IdleDetector {
	if waylandSession() {
		d, err := NewSwayIdleDetector(cfg.IdleThreshold.Duration, logger)
		if err != nil {
			logger.Warn().Err(err).
				Msg("idle detection disabled; install swayidle for Wayland idle detection")
			return NopIdleDetector{}
		}
		return d
	}

	d, err := NewXIdleDetector()
	if err != nil {
		logger.Warn().Err(err).Msg("idle detection disabled")
		return NopIdleDetector{}
	}
	return d
}
