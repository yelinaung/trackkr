package tracker

import (
	"context"
	"testing"
	"time"
)

// A lid close is a wall clock that has moved and a monotonic clock that has
// not. The record must describe the time the machine was awake, not the time
// that passed, or an overnight sleep is charted as overnight use of whichever
// application happened to be frontmost.
func TestTrackerDoesNotRecordTimeSpentSuspended(t *testing.T) {
	t.Parallel()
	const awake = 63 * time.Second
	const asleep = 2 * time.Hour

	wd := &mockWindowDetector{
		calls: []mockWindowCall{
			{info: WindowInfo{AppName: testFirefoxApp, Title: "Chrome"}},
			// The window title changes on wake, which is what closes the
			// segment: the machine was woken by a keypress, so the idle
			// detector reports nothing and the idle path never runs.
			{info: WindowInfo{AppName: testFirefoxApp, Title: testMainGoTitle}},
		},
	}
	id := &mockIdleDetector{times: []time.Duration{0, 0}}

	trk, reporter := newTestTracker(t, wd, id)
	trk.elapsed = func(time.Time, time.Time) time.Duration { return awake }

	trk.poll(context.Background())
	startedAt := trk.current.StartedAt
	// Only the wall clock advances across the sleep.
	trk.current.StartedAt = startedAt.Add(-(awake + asleep))

	trk.poll(context.Background())

	if reporter.QueueLen() != 1 {
		t.Fatalf("queue len = %d, want the suspended segment finalized once", reporter.QueueLen())
	}
	rec := reporter.queue[0]

	if rec.DurationS != int(awake.Seconds()) {
		t.Errorf("duration = %ds, want the %s the machine was awake", rec.DurationS, awake)
	}
	if span := rec.EndedAt.Sub(rec.StartedAt); span != awake {
		t.Errorf("interval = %s, want %s: the sleep belongs between records, not inside one", span, awake)
	}
}

// The interval and the duration are two descriptions of one span, and the
// dashboard reads the interval. Nothing the tracker emits may let them drift.
func TestTrackerRecordIntervalAlwaysMatchesDuration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
	}{
		{name: "ordinary segment", elapsed: 45 * time.Second},
		{name: "sub-second remainder", elapsed: 45*time.Second + 700*time.Millisecond},
		{name: "long segment", elapsed: 3 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wd := &mockWindowDetector{
				calls: []mockWindowCall{
					{info: WindowInfo{AppName: testFirefoxApp, Title: "Chrome"}},
					{info: WindowInfo{AppName: testVSCodeApp, Title: testMainGoTitle}},
				},
			}
			trk, reporter := newTestTracker(t, wd, &mockIdleDetector{times: []time.Duration{0, 0}})
			trk.elapsed = func(time.Time, time.Time) time.Duration { return tc.elapsed }

			trk.poll(context.Background())
			trk.poll(context.Background())

			if reporter.QueueLen() != 1 {
				t.Fatalf("queue len = %d, want 1", reporter.QueueLen())
			}
			rec := reporter.queue[0]

			// The duration truncates to whole seconds, so the interval may
			// carry a remainder the duration cannot.
			span := rec.EndedAt.Sub(rec.StartedAt)
			if drift := span - time.Duration(rec.DurationS)*time.Second; drift < 0 || drift >= time.Second {
				t.Errorf("interval %s and duration %ds drift by %s", span, rec.DurationS, drift)
			}
		})
	}
}
