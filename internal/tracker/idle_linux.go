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
	out, err := exec.CommandContext(ctx, x.xprintidlePath).Output() //nolint:gosec // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- path validated by LookPath
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

// NewIdleDetectorOrNop tries to create an XIdleDetector. If
// xprintidle is not found, it logs a warning and returns a
// NopIdleDetector.
func NewIdleDetectorOrNop(logger *zerolog.Logger) IdleDetector {
	d, err := NewXIdleDetector()
	if err != nil {
		logger.Warn().Err(err).Msg("idle detection disabled")
		return NopIdleDetector{}
	}
	return d
}
