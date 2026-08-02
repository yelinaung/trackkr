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

// IdleAvailable reports that this detector measures nothing.
//
// Zero idle and no error is indistinguishable from a present user, and
// anything serving that answer onward would say the user is here for as
// long as the platform detector stays missing. A browser told that
// holds its tab segment open indefinitely, which is worse than being
// told nothing: told nothing, it falls back to its own reckoning.
func (NopIdleDetector) IdleAvailable() bool { return false }

// idleAvailability is implemented by detectors that can say they are
// not really detecting. A detector without the method is taken at its
// word, since every real one measures something.
type idleAvailability interface {
	IdleAvailable() bool
}

// idleUsable reports whether a detector's answers mean anything.
func idleUsable(d IdleDetector) bool {
	if d == nil {
		return false
	}
	if a, ok := d.(idleAvailability); ok {
		return a.IdleAvailable()
	}
	return true
}
