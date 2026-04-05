package tracker

import (
	"context"
	"time"
)

// IdleDetector reports how long the user has been idle.
type IdleDetector interface {
	IdleTime(ctx context.Context) (time.Duration, error)
}

// NopIdleDetector always reports zero idle time. Used when the
// platform idle detector (e.g. xprintidle) is unavailable.
type NopIdleDetector struct{}

func (NopIdleDetector) IdleTime(_ context.Context) (time.Duration, error) {
	return 0, nil
}
