package tracker

import (
	"context"
	"errors"
)

// ErrNoActiveWindow is returned when no trackable window has focus.
var ErrNoActiveWindow = errors.New("no active window")

// ErrUnsupportedPlatform is returned when the current platform has no
// window detector implementation.
var ErrUnsupportedPlatform = errors.New("window detection not supported on this platform")

// WindowInfo holds the currently focused window's metadata.
type WindowInfo struct {
	AppName string
	Title   string
}

// WindowDetector returns the currently active window.
type WindowDetector interface {
	ActiveWindow(ctx context.Context) (WindowInfo, error)
}
