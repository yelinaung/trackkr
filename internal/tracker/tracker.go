package tracker

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog"

	"github.com/yelinaung/trackkr/internal/identity"
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
	appIcons map[string]queuedAppIcon

	// elapsed measures how long a segment ran. It is a field only so a test
	// can reproduce a machine suspend: that state is a wall clock which has
	// advanced while the monotonic clock has not, and the standard library
	// offers no way to build a pair of times that disagree that way.
	elapsed func(startedAt, endedAt time.Time) time.Duration
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
		appIcons: make(map[string]queuedAppIcon),
		elapsed:  monotonicElapsed,
	}
}

// monotonicElapsed measures a segment by the monotonic reading both times
// carry, which is what Sub already prefers. Naming it makes the choice
// deliberate rather than incidental.
func monotonicElapsed(startedAt, endedAt time.Time) time.Duration {
	return endedAt.Sub(startedAt)
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
	if !noWindow {
		t.maybeEnqueueAppIcon(info, now)
	}

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

// finalize closes the open segment. The end is derived from how long the
// process was actually running rather than from the wall clock it was handed.
//
// time.Time carries a monotonic reading alongside the wall clock, and Sub
// prefers the monotonic one -- which does not advance while the machine is
// suspended. So on a lid close the two disagree: an hour of sleep moves the
// wall clock an hour and the monotonic clock not at all. Taking the end as
// start plus the monotonic elapsed time keeps EndedAt and DurationS describing
// the same span, and leaves the sleep as a gap between records rather than
// inside one.
//
// This is not covered by the idle backdating in poll. That only runs on the
// transition into idle, and the first poll after a wake sees almost no idle
// time, because the machine was woken by a keypress. The gap is invisible to a
// loop whose own clock skipped it.
//
// Times built without a monotonic reading -- anything from time.Date, so every
// test fixture -- subtract by wall clock, and this is then exactly the previous
// behaviour.
func (t *Tracker) finalize(endedAt time.Time) {
	if t.current == nil {
		return
	}

	elapsed := t.elapsed(t.current.StartedAt, endedAt)
	dur := int(elapsed.Seconds())
	if dur <= 0 {
		t.current = nil
		return
	}
	endedAt = t.current.StartedAt.Add(elapsed)

	// A fresh identity per finalized segment. A generator failure is not
	// worth dropping the record over: ensureIdentity derives a stable ID
	// from the content instead.
	recordID, err := identity.New()
	if err != nil {
		t.logger.Warn().Err(err).Msg("falling back to a derived record id")
		recordID = ""
	}

	rec := Record{
		RecordID:  recordID,
		Producer:  identity.ProducerDesktop,
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
