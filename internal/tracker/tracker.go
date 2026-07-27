package tracker

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog"
)

type state int

const (
	stateTracking state = iota
	stateIdle
)

// Tracker is the main activity tracking loop.
type Tracker struct {
	cfg      *Config
	window   WindowDetector
	idle     IdleDetector
	reporter *Reporter
	logger   *zerolog.Logger
	state    state
	current  *activeRecord
}

type activeRecord struct {
	AppName   string
	Title     string
	StartedAt time.Time
}

// NewTracker creates a tracker wired to the given components.
func NewTracker(
	cfg *Config,
	w WindowDetector,
	i IdleDetector,
	r *Reporter,
	logger *zerolog.Logger,
) *Tracker {
	return &Tracker{
		cfg:      cfg,
		window:   w,
		idle:     i,
		reporter: r,
		logger:   logger,
		state:    stateTracking,
	}
}

// Run starts the tracking loop. It blocks until ctx is cancelled,
// finalizing the in-flight record on the way out. Poll errors are
// logged and retried on the next tick, so there is nothing for a
// caller to handle.
func (t *Tracker) Run(ctx context.Context) {
	ticker := time.NewTicker(t.cfg.PollInterval.Duration)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			t.finalizeIfActive(time.Now())
			return
		case <-ticker.C:
			t.poll(ctx)
		}
	}
}

func (t *Tracker) poll(ctx context.Context) {
	idleTime, err := t.idle.IdleTime(ctx)
	if err != nil {
		t.logger.Warn().Err(err).Msg("idle detection error")
	}

	info, err := t.window.ActiveWindow(ctx)
	noWindow := errors.Is(err, ErrNoActiveWindow)
	if err != nil && !noWindow {
		t.logger.Warn().Err(err).Msg("window detection error")
		return
	}

	now := time.Now()

	switch t.state {
	case stateTracking:
		if noWindow || idleTime >= t.cfg.IdleThreshold.Duration {
			// Transition to idle.
			endedAt := now.Add(-idleTime)
			t.finalizeIfActive(endedAt)
			t.state = stateIdle
			t.logger.Debug().Dur("idle", idleTime).Msg("entered idle state")
			return
		}

		if t.current == nil {
			t.startNew(info, now)
			return
		}

		if t.current.AppName != info.AppName || t.current.Title != info.Title {
			t.finalize(now)
			t.startNew(info, now)
		}

	case stateIdle:
		if !noWindow && idleTime < t.cfg.IdleThreshold.Duration {
			t.startNew(info, now)
			t.state = stateTracking
			t.logger.Debug().Msg("resumed from idle")
		}
	}
}

func (t *Tracker) startNew(info WindowInfo, now time.Time) {
	t.current = &activeRecord{
		AppName:   info.AppName,
		Title:     info.Title,
		StartedAt: now,
	}
	t.logger.Debug().
		Str("app", info.AppName).
		Str("title", info.Title).
		Msg("tracking new window")
}

func (t *Tracker) finalize(endedAt time.Time) {
	if t.current == nil {
		return
	}

	dur := int(endedAt.Sub(t.current.StartedAt).Seconds())
	if dur <= 0 {
		t.current = nil
		return
	}

	rec := Record{
		AppName:   t.current.AppName,
		Title:     t.current.Title,
		StartedAt: t.current.StartedAt,
		EndedAt:   endedAt,
		DurationS: dur,
	}
	t.reporter.Enqueue(&rec)
	t.current = nil
}

func (t *Tracker) finalizeIfActive(endedAt time.Time) {
	if t.current != nil {
		t.finalize(endedAt)
	}
}
