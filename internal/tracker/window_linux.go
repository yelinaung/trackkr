//go:build linux

package tracker

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/icon"
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
func NewWindowDetector(cfg *Config, logger *zerolog.Logger) (WindowDetector, error) {
	// One cache serves whichever detector is returned. It owns a worker
	// goroutine, so whatever holds it must close it.
	icons := newLinuxAppIcons(cfg)

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
			icons.Close()
			return nil, fmt.Errorf("%w: %w", ErrUnsupportedPlatform, err)
		}
		d.icons = icons
		return d, nil
	}

	d, err := NewXWindowDetector()
	if err != nil {
		icons.Close()
		return nil, err
	}
	d.icons = icons
	return d, nil
}

// newLinuxAppIcons wires the freedesktop resolver behind the same cache
// macOS uses, so the dedup, expiry and worker queue are shared rather
// than reimplemented per platform.
//
// The cache key is {PID, Key} and Linux passes PID 0 deliberately.
// Sway's tree does carry a pid, so plumbing it through looks tempting,
// but it earns its place only on macOS, where resolution goes through
// the running process's bundle and a relaunch genuinely needs redoing.
// Freedesktop resolution is name-based, so a PID in the key would
// re-walk the filesystem for an identical answer on every restart.
func newLinuxAppIcons(cfg *Config) *appIconCache {
	theme := ""
	if cfg != nil {
		theme = cfg.IconTheme
	}
	resolver := newIconResolver(xdgIconRoots(), theme)
	return newAppIconCache(time.Now, func(ctx context.Context, app appInfo) *icon.App {
		return resolver.resolve(ctx, icon.AppKey(app.Name))
	})
}

// XWindowDetector uses xdotool and xprop to detect active windows
// on X11.
type XWindowDetector struct {
	xdotoolPath string
	xpropPath   string
	// icons resolves application artwork, or nil to report none.
	icons *appIconCache
}

// Close stops the icon worker. The daemon closes detectors through a
// duck-typed interface{ Close() }, so without this the worker outlives
// every X11 session for the life of the process.
func (x *XWindowDetector) Close() {
	if x.icons != nil {
		x.icons.Close()
	}
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

	return WindowInfo{AppName: appName, Title: title, AppIcon: x.appIcon(ctx, appName)}, nil
}

// appIcon resolves artwork for a window, or nil when the detector has no
// cache or the desktop offers nothing usable.
func (x *XWindowDetector) appIcon(ctx context.Context, appName string) *icon.App {
	if x.icons == nil {
		return nil
	}
	return x.icons.iconForApp(ctx, appInfo{Name: appName})
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
