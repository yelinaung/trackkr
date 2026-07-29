//go:build !linux && !darwin

package tracker

import (
	"fmt"
	"runtime"

	"github.com/rs/zerolog"
)

// NewWindowDetector reports that this platform has no window detector.
func NewWindowDetector(_ *Config, _ *zerolog.Logger) (WindowDetector, error) {
	return nil, fmt.Errorf("%w: %s", ErrUnsupportedPlatform, runtime.GOOS)
}
