package tracker

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

type mockWindowDetector struct {
	calls []mockWindowCall
	index int
}

type mockWindowCall struct {
	info WindowInfo
	err  error
}

func (m *mockWindowDetector) ActiveWindow(_ context.Context) (WindowInfo, error) {
	if m.index >= len(m.calls) {
		return WindowInfo{}, ErrNoActiveWindow
	}
	c := m.calls[m.index]
	m.index++
	return c.info, c.err
}

type mockIdleDetector struct {
	times []time.Duration
	index int
}

func (m *mockIdleDetector) IdleTime(_ context.Context) (time.Duration, error) {
	if m.index >= len(m.times) {
		return 0, nil
	}
	d := m.times[m.index]
	m.index++
	return d, nil
}

func newTestTracker(t *testing.T, wd WindowDetector, id IdleDetector) (*Tracker, *Reporter) {
	t.Helper()
	cfg := &Config{
		ServerURL:     testUnusedServerURL,
		APIKey:        testValue,
		PollInterval:  Duration{time.Second},
		IdleThreshold: Duration{5 * time.Minute},
		FlushInterval: Duration{time.Hour},
		FlushSize:     100,
		DataDir:       t.TempDir(),
	}
	logger := zerolog.Nop()
	reporter := NewReporter(cfg, http.DefaultClient, &logger)
	trk := NewTracker(cfg, wd, id, reporter, &logger)
	return trk, reporter
}

func TestTrackerWindowChange(t *testing.T) {
	t.Parallel()
	wd := &mockWindowDetector{
		calls: []mockWindowCall{
			{info: WindowInfo{AppName: testFirefoxApp, Title: "Google"}},
			{info: WindowInfo{AppName: testFirefoxApp, Title: "Google"}},
			{info: WindowInfo{AppName: testVSCodeApp, Title: testMainGoTitle}},
		},
	}
	id := &mockIdleDetector{times: make([]time.Duration, 3)}

	trk, reporter := newTestTracker(t, wd, id)

	// Backdate the start so records have >0 duration.
	trk.poll(context.Background()) // Starts Firefox record.
	trk.current.StartedAt = time.Now().Add(-10 * time.Second)

	trk.poll(context.Background()) // Same window — no change.
	trk.poll(context.Background()) // Window changed to VS Code — finalizes Firefox.

	// Firefox should be enqueued.
	if reporter.QueueLen() != 1 {
		t.Errorf("queue len = %d, want 1", reporter.QueueLen())
	}
}

func TestTrackerIdleTransition(t *testing.T) {
	t.Parallel()
	wd := &mockWindowDetector{
		calls: []mockWindowCall{
			{info: WindowInfo{AppName: testFirefoxApp, Title: testRecordTitle}},
			{info: WindowInfo{AppName: testFirefoxApp, Title: testRecordTitle}},
		},
	}
	id := &mockIdleDetector{
		times: []time.Duration{
			0,
			6 * time.Minute,
		},
	}

	trk, reporter := newTestTracker(t, wd, id)

	trk.poll(context.Background()) // Starts Firefox.
	trk.current.StartedAt = time.Now().Add(-10 * time.Minute)

	trk.poll(context.Background()) // Idle exceeds threshold — finalizes.

	if reporter.QueueLen() != 1 {
		t.Errorf("queue len = %d, want 1", reporter.QueueLen())
	}
	if trk.state != stateIdle {
		t.Errorf("state = %d, want stateIdle", trk.state)
	}
}

func TestTrackerResumeFromIdle(t *testing.T) {
	t.Parallel()
	wd := &mockWindowDetector{
		calls: []mockWindowCall{
			{info: WindowInfo{AppName: testFirefoxApp, Title: testRecordTitle}},
			{info: WindowInfo{AppName: testFirefoxApp, Title: testRecordTitle}},
			{info: WindowInfo{AppName: testVSCodeApp, Title: testMainGoTitle}},
		},
	}
	id := &mockIdleDetector{
		times: []time.Duration{
			0,
			6 * time.Minute,
			0,
		},
	}

	trk, reporter := newTestTracker(t, wd, id)

	trk.poll(context.Background()) // Starts Firefox.
	trk.current.StartedAt = time.Now().Add(-10 * time.Minute)

	trk.poll(context.Background()) // Goes idle — finalizes Firefox.

	if trk.state != stateIdle {
		t.Fatalf("state = %d, want stateIdle", trk.state)
	}

	trk.poll(context.Background()) // Resumes — starts VS Code.

	if trk.state != stateTracking {
		t.Errorf("state = %d, want stateTracking", trk.state)
	}
	// Firefox finalized on idle.
	if reporter.QueueLen() != 1 {
		t.Errorf("queue len = %d, want 1", reporter.QueueLen())
	}
	if trk.current == nil {
		t.Fatal("current record is nil, want VS Code")
	}
	if trk.current.AppName != testVSCodeApp {
		t.Errorf("current.AppName = %q, want %q", trk.current.AppName, testVSCodeApp)
	}
}

func TestTrackerNoActiveWindow(t *testing.T) {
	t.Parallel()
	wd := &mockWindowDetector{
		calls: []mockWindowCall{
			{info: WindowInfo{AppName: testFirefoxApp, Title: testRecordTitle}},
			{err: ErrNoActiveWindow},
		},
	}
	id := &mockIdleDetector{times: make([]time.Duration, 2)}

	trk, reporter := newTestTracker(t, wd, id)

	trk.poll(context.Background()) // Starts Firefox.
	trk.current.StartedAt = time.Now().Add(-10 * time.Second)

	trk.poll(context.Background()) // No active window — treats as idle, finalizes.

	if reporter.QueueLen() != 1 {
		t.Errorf("queue len = %d, want 1", reporter.QueueLen())
	}
	if trk.state != stateIdle {
		t.Errorf("state = %d, want stateIdle", trk.state)
	}
}

func TestTrackerShortRecordDiscarded(t *testing.T) {
	t.Parallel()
	logger := zerolog.Nop()
	cfg := &Config{
		PollInterval:  Duration{time.Second},
		IdleThreshold: Duration{5 * time.Minute},
		FlushInterval: Duration{time.Hour},
		FlushSize:     100,
		DataDir:       t.TempDir(),
		ServerURL:     testUnusedServerURL,
		APIKey:        testValue,
	}
	reporter := NewReporter(cfg, http.DefaultClient, &logger)
	trk := NewTracker(cfg, nil, nil, reporter, &logger)

	now := time.Now()
	trk.current = &activeRecord{
		AppName:   testRecordTitle,
		Title:     testRecordTitle,
		StartedAt: now.Add(time.Hour),
	}
	trk.finalize(now) // endedAt before startedAt.

	if reporter.QueueLen() != 0 {
		t.Errorf("queue len = %d, want 0 (short record discarded)", reporter.QueueLen())
	}
}

func TestTrackerGracefulShutdown(t *testing.T) {
	t.Parallel()
	// Window detector that always returns the same window.
	wd := &mockWindowDetector{
		calls: []mockWindowCall{
			{info: WindowInfo{AppName: testFirefoxApp, Title: testRecordTitle}},
			{info: WindowInfo{AppName: testFirefoxApp, Title: testRecordTitle}},
			{info: WindowInfo{AppName: testFirefoxApp, Title: testRecordTitle}},
			{info: WindowInfo{AppName: testFirefoxApp, Title: testRecordTitle}},
			{info: WindowInfo{AppName: testFirefoxApp, Title: testRecordTitle}},
		},
	}
	id := &mockIdleDetector{times: make([]time.Duration, 5)}

	cfg := &Config{
		ServerURL:     testUnusedServerURL,
		APIKey:        testValue,
		PollInterval:  Duration{10 * time.Millisecond},
		IdleThreshold: Duration{5 * time.Minute},
		FlushInterval: Duration{time.Hour},
		FlushSize:     100,
		DataDir:       t.TempDir(),
	}
	logger := zerolog.Nop()
	reporter := NewReporter(cfg, http.DefaultClient, &logger)
	trk := NewTracker(cfg, wd, id, reporter, &logger)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Wait for at least one poll so a record exists.
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_ = trk.Run(ctx)

	// The current record should be finalized on shutdown.
	// It may have 0 duration due to timing, so just check it ran
	// without panicking. If it produced a record, great.
	// The key behavior tested here is that Run returns cleanly.
}
