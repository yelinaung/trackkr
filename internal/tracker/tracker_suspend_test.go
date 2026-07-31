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

// Working in the same window either side of a sleep is the case nothing else
// closes: no title change, no idle transition. The resumed work must be
// recorded when it happened, not folded onto the end of the pre-sleep segment
// -- which would draw it before the gap, and on the previous day if the sleep
// crossed midnight.
func TestTrackerSplitsSegmentOnResume(t *testing.T) {
	t.Parallel()
	const awake = 30 * time.Second
	const asleep = 3 * time.Hour

	same := WindowInfo{AppName: testFirefoxApp, Title: testMainGoTitle}
	wd := &mockWindowDetector{
		calls: []mockWindowCall{{info: same}, {info: same}, {info: same}},
	}
	trk, reporter := newTestTracker(t, wd, &mockIdleDetector{times: []time.Duration{0, 0, 0}})
	// The process experiences the poll interval; the wall clock is moved by
	// the test to stand in for the sleep.
	trk.elapsed = func(time.Time, time.Time) time.Duration { return awake }

	trk.poll(context.Background()) // Starts the pre-sleep segment.
	preSleepStart := trk.current.StartedAt

	trk.poll(context.Background()) // Still awake, same window: nothing closes.
	if reporter.QueueLen() != 0 {
		t.Fatalf("queue len = %d, want nothing finalized while awake", reporter.QueueLen())
	}

	// The sleep: only the wall clock moves.
	lastAwake := trk.lastPoll
	trk.lastPoll = trk.lastPoll.Add(-asleep)
	trk.current.StartedAt = trk.current.StartedAt.Add(-asleep)
	preSleepStart = preSleepStart.Add(-asleep)

	trk.poll(context.Background()) // First poll after the resume.

	if reporter.QueueLen() != 1 {
		t.Fatalf("queue len = %d, want the pre-sleep segment closed on resume", reporter.QueueLen())
	}
	rec := reporter.queue[0]
	if !rec.StartedAt.Equal(preSleepStart) {
		t.Errorf("closed segment starts at %s, want the pre-sleep start %s", rec.StartedAt, preSleepStart)
	}
	if span := rec.EndedAt.Sub(rec.StartedAt); span != awake {
		t.Errorf("closed segment spans %s, want the %s before the sleep", span, awake)
	}

	// The resumed work is a new segment timed from the wake, not a
	// continuation backdated to before the sleep.
	if trk.current == nil {
		t.Fatal("no segment tracking the resumed window")
	}
	if !trk.current.StartedAt.After(lastAwake) {
		t.Errorf("resumed segment starts at %s, want a time after the sleep", trk.current.StartedAt)
	}
	if !trk.current.StartedAt.After(rec.EndedAt) {
		t.Error("resumed segment overlaps the segment that ended before the sleep")
	}
}

// A slow poll is not a suspend: both clocks are delayed together, so nothing
// should be split.
func TestTrackerDoesNotSplitOnOrdinaryPollDelay(t *testing.T) {
	t.Parallel()
	same := WindowInfo{AppName: testFirefoxApp, Title: testMainGoTitle}
	wd := &mockWindowDetector{calls: []mockWindowCall{{info: same}, {info: same}}}
	trk, reporter := newTestTracker(t, wd, &mockIdleDetector{times: []time.Duration{0, 0}})

	trk.poll(context.Background())
	// A long gap the process did experience: wall and elapsed agree.
	trk.elapsed = func(startedAt, endedAt time.Time) time.Duration {
		return endedAt.Round(0).Sub(startedAt.Round(0))
	}
	trk.lastPoll = trk.lastPoll.Add(-time.Hour)
	trk.current.StartedAt = trk.current.StartedAt.Add(-time.Hour)

	trk.poll(context.Background())

	if reporter.QueueLen() != 0 {
		t.Errorf("queue len = %d, want no split for a delay the process experienced", reporter.QueueLen())
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
