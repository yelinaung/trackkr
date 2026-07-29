//go:build darwin && !cgo

package tracker

import (
	"fmt"
	"runtime"

	"github.com/rs/zerolog"
)

// NewWindowDetector reports that native macOS detection requires cgo.
func NewWindowDetector(_ *Config, _ *zerolog.Logger) (WindowDetector, error) {
	return nil, fmt.Errorf("%w: %s build requires cgo", ErrUnsupportedPlatform, runtime.GOOS)
}

// NewIdleDetectorOrNop falls back to no idle detection without cgo.
func NewIdleDetectorOrNop(_ *Config, logger *zerolog.Logger) IdleDetector {
	if logger != nil {
		logger.Info().Msg("idle detection unavailable in a macOS build without cgo")
	}
	return NopIdleDetector{}
}
