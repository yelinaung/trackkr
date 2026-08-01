//go:build linux

package tracker

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/rs/zerolog"
)

// NewWindowDetector returns the platform's window detector: sway's IPC
// on a sway session, xdotool and xprop on X11.
//
// A Wayland session that is not sway gets neither. xdotool does not
// fail there -- it asks XWayland, which answers about whichever X
// client it last saw focused, so a native Wayland window is reported as
// something else entirely and reported with full confidence. A wrong
// window recorded that way is worse than no window at all, so an
// unsupported compositor says so instead.
func NewWindowDetector(_ *Config, logger *zerolog.Logger) (WindowDetector, error) {
	// Return an explicit nil interface on failure; returning the
	// typed nil pointer directly would make the interface non-nil.
	// Any Wayland session gets the sway detector tried, not just one
	// advertising SWAYSOCK. sway does not export that variable to the
	// systemd user manager itself, so requiring it would refuse to
	// start on the ordinary case -- a sway session whose daemon runs
	// as a user unit -- with a live socket sitting in the runtime
	// directory. A compositor that is not sway has no such socket to
	// find, so it still ends up at the error below.
	if waylandSession() {
		d, err := NewSwayWindowDetector(logger)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrUnsupportedPlatform, err)
		}
		return d, nil
	}

	d, err := NewXWindowDetector()
	if err != nil {
		return nil, err
	}
	return d, nil
}

// XWindowDetector uses xdotool and xprop to detect active windows
// on X11.
type XWindowDetector struct {
	xdotoolPath string
	xpropPath   string
}

// NewXWindowDetector creates a detector, verifying that xdotool and
// xprop are installed.
func NewXWindowDetector() (*XWindowDetector, error) {
	xdotool, err := exec.LookPath("xdotool")
	if err != nil {
		return nil, fmt.Errorf("xdotool not found: %w", err)
	}
	xprop, err := exec.LookPath("xprop")
	if err != nil {
		return nil, fmt.Errorf("xprop not found: %w", err)
	}
	return &XWindowDetector{xdotoolPath: xdotool, xpropPath: xprop}, nil
}

// ActiveWindow returns the currently focused window's app name and
// title.
func (x *XWindowDetector) ActiveWindow(ctx context.Context) (WindowInfo, error) {
	// Get window ID.
	idOut, err := exec.CommandContext(ctx, x.xdotoolPath, "getactivewindow").Output() //nolint:gosec // nosemgrep // gitlab-advanced-sast-exclude -- path validated by LookPath
	if err != nil {
		return WindowInfo{}, ErrNoActiveWindow
	}
	windowID := strings.TrimSpace(string(idOut))
	if windowID == "" {
		return WindowInfo{}, ErrNoActiveWindow
	}

	// Get window title.
	titleOut, err := exec.CommandContext(ctx, x.xdotoolPath, "getactivewindow", "getwindowname").Output() //nolint:gosec // nosemgrep // gitlab-advanced-sast-exclude -- path validated by LookPath
	if err != nil {
		return WindowInfo{}, fmt.Errorf("getting window name: %w", err)
	}
	title := strings.TrimSpace(string(titleOut))

	// Get WM_CLASS for app name.
	classOut, err := exec.CommandContext(ctx, x.xpropPath, "-id", windowID, "WM_CLASS").Output() //nolint:gosec // nosemgrep // gitlab-advanced-sast-exclude -- paths validated by LookPath
	appName := unknownApp
	if err == nil {
		appName = parseWMClass(string(classOut))
	}

	return WindowInfo{AppName: appName, Title: title}, nil
}

// parseWMClass extracts the application name from xprop WM_CLASS
// output. Example input:
//
//	WM_CLASS(STRING) = "Navigator", "firefox"
//
// Returns the second value ("firefox") as the app name.
func parseWMClass(output string) string {
	// Find the part after "="
	parts := strings.SplitN(output, "=", 2)
	if len(parts) < 2 {
		return unknownApp
	}

	values := strings.Split(parts[1], ",")
	// Prefer the second value (class name), fall back to first
	// (instance name).
	idx := 0
	if len(values) >= 2 {
		idx = 1
	}

	name := strings.TrimSpace(values[idx])
	name = strings.Trim(name, `"`)
	if name == "" {
		return unknownApp
	}
	return name
}
