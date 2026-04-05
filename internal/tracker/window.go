package tracker

import (
	"context"
	"errors"
)

// ErrNoActiveWindow is returned when no window has focus (e.g.
// locked screen, screensaver).
var ErrNoActiveWindow = errors.New("no active window")

// WindowInfo holds the currently focused window's metadata.
type WindowInfo struct {
	AppName string
	Title   string
}

// WindowDetector returns the currently active window.
type WindowDetector interface {
	ActiveWindow(ctx context.Context) (WindowInfo, error)
}
