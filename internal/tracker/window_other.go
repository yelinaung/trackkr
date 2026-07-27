//go:build !linux

package tracker

import (
	"errors"
	"fmt"
	"runtime"
)

// ErrUnsupportedPlatform is returned when the current platform has no
// window detector implementation.
var ErrUnsupportedPlatform = errors.New("window detection not supported on this platform")

// NewWindowDetector returns the platform's window detector. Only
// Linux/X11 is implemented so far; macOS support is planned.
func NewWindowDetector() (WindowDetector, error) {
	return nil, fmt.Errorf("%w: %s", ErrUnsupportedPlatform, runtime.GOOS)
}
