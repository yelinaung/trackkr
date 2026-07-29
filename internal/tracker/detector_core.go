package tracker

import (
	"context"
	"errors"
	"strings"
)

const (
	statusOK int = iota
	statusNoApp
	statusFailed
)

var errFrontmostFailed = errors.New("querying frontmost application failed")

type appInfo struct {
	Name string
	PID  int
}

type detectorCore struct {
	policy    *titlePolicy
	frontmost func() (*appInfo, error)
	titleFor  func(int) string
}

func (d *detectorCore) ActiveWindow(ctx context.Context) (WindowInfo, error) {
	if err := ctx.Err(); err != nil {
		return WindowInfo{}, err
	}

	app, err := d.frontmost()
	if err != nil {
		return WindowInfo{}, err
	}
	if app == nil || strings.TrimSpace(app.Name) == "" {
		return WindowInfo{}, ErrNoActiveWindow
	}
	if err := ctx.Err(); err != nil {
		return WindowInfo{}, err
	}

	info := WindowInfo{AppName: app.Name}
	if d.policy == nil || !d.policy.canRead() {
		return info, nil
	}
	if err := ctx.Err(); err != nil {
		return WindowInfo{}, err
	}
	info.Title = d.titleFor(app.PID)
	return info, nil
}

func mapFrontmost(status int, app appInfo) (*appInfo, error) {
	switch status {
	case statusOK:
		return &app, nil
	case statusNoApp:
		return nil, nil //nolint:nilnil // Nil app is the explicit no-active-window state.
	default:
		return nil, errFrontmostFailed
	}
}
