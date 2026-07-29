//go:build darwin && cgo

package tracker

/*
#cgo LDFLAGS: -framework Foundation -framework ApplicationServices -framework CoreGraphics
#include <stdlib.h>
#include "macos_darwin.h"
*/
import "C"

import (
	"errors"
	"time"
	"unsafe"

	"github.com/rs/zerolog"
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

	return &detectorCore{
		policy: policy,
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
