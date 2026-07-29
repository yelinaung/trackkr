//go:build darwin && cgo

package tracker

/*
#cgo LDFLAGS: -framework CoreGraphics
#include <CoreGraphics/CoreGraphics.h>
*/
import "C"

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/rs/zerolog"
)

type darwinIdleDetector struct{}

// NewIdleDetectorOrNop returns the CoreGraphics idle detector on macOS.
func NewIdleDetectorOrNop(_ *Config, _ *zerolog.Logger) IdleDetector {
	return darwinIdleDetector{}
}

func (darwinIdleDetector) IdleTime(ctx context.Context) (time.Duration, error) {
	err := ctx.Err()
	if err != nil {
		return 0, err
	}

	seconds := float64(C.CGEventSourceSecondsSinceLastEventType(
		C.kCGEventSourceStateHIDSystemState,
		C.kCGAnyInputEventType,
	))
	if seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, fmt.Errorf("invalid macOS idle time: %v seconds", seconds)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}
