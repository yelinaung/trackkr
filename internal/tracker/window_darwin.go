//go:build darwin && cgo

package tracker

/*
#cgo LDFLAGS: -framework Foundation -framework AppKit -framework ApplicationServices -framework CoreGraphics
#include <stdlib.h>
#include "macos_darwin.h"

_Static_assert(sizeof(pid_t) <= sizeof(int), "pid_t must fit in Go int");

static int trackkr_app_icon_png_go(pid_t pid, trackkr_png *out) {
    return trackkr_app_icon_png(pid, out) ? 1 : 0;
}
*/
import "C"

import (
	"context"
	"errors"
	"time"
	"unsafe"

	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/icon"
)

var (
	_ = [1]struct{}{}[statusOK-int(C.TRACKKR_OK)]
	_ = [1]struct{}{}[statusNoApp-int(C.TRACKKR_NO_APP)]
	_ = [1]struct{}{}[statusFailed-int(C.TRACKKR_FAILED)]
)

// NewWindowDetector creates the macOS window detector.
func NewWindowDetector(cfg *Config, logger *zerolog.Logger) (WindowDetector, error) {
	if cfg == nil {
		return nil, errors.New("creating macOS window detector: nil config")
	}

	if cfg.MacOSReadTitles && cfg.PollInterval.Duration < 2*time.Second && logger != nil {
		logger.Warn().
			Dur("poll_interval", cfg.PollInterval.Duration).
			Msg("window title reads can take about one second; consider a poll interval of at least 2s")
	}

	policy := newTitlePolicy(
		cfg,
		func() bool {
			return bool(C.trackkr_is_accessibility_trusted())
		},
		func() {
			C.trackkr_prompt_for_accessibility()
		},
		func(trusted bool) {
			if logger == nil {
				return
			}
			if trusted {
				logger.Info().Msg("Accessibility permission granted; window titles enabled")
				return
			}
			logger.Warn().Msg("Accessibility permission not granted; recording application names without titles")
		},
	)
	loadAppIcon := loadDarwinAppIcon
	if logger != nil {
		loadAppIcon = func(ctx context.Context, app appInfo) *icon.App {
			appIcon := loadDarwinAppIcon(ctx, app)
			if appIcon == nil && ctx.Err() == nil {
				logger.Debug().
					Str("app", app.Name).
					Int("pid", app.PID).
					Msg("application icon unavailable")
			}
			return appIcon
		}
	}
	iconCache := newAppIconCache(time.Now, loadAppIcon)

	return &detectorCore{
		policy:    policy,
		iconFor:   iconCache.iconForApp,
		closeIcon: iconCache.Close,
		frontmost: func() (*appInfo, error) {
			var native C.trackkr_app
			status := int(C.trackkr_frontmost_app(&native)) //nolint:gocritic // cgo expands this call into generated expressions.
			if native.name != nil {
				defer C.free(unsafe.Pointer(native.name))
			}

			app := appInfo{PID: int(native.pid)}
			if native.name != nil {
				app.Name = C.GoString(native.name)
			}
			return mapFrontmost(status, app)
		},
		titleFor: func(pid int) string {
			title := C.trackkr_focused_window_title(C.pid_t(pid))
			if title == nil {
				return ""
			}
			defer C.free(unsafe.Pointer(title))
			return C.GoString(title)
		},
	}, nil
}

func loadDarwinAppIcon(ctx context.Context, app appInfo) *icon.App {
	if err := ctx.Err(); err != nil {
		return nil
	}

	var native C.trackkr_png
	if int(C.trackkr_app_icon_png_go(C.pid_t(app.PID), &native)) == 0 { //nolint:gocritic // cgo expands this call into generated expressions.
		return nil
	}
	if native.name != nil {
		defer C.free(unsafe.Pointer(native.name))
	}
	if native.bytes != nil {
		defer C.free(unsafe.Pointer(native.bytes))
	}
	if native.name == nil || native.bytes == nil || native.length == 0 || native.length > C.size_t(icon.MaxPNGBytes) {
		return nil
	}

	return appIconForObservedProcess(
		app.Name,
		C.GoString(native.name),
		C.GoBytes(unsafe.Pointer(native.bytes), C.int(native.length)),
	)
}
