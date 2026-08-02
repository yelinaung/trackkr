//go:build linux

package tracker

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fixedClock returns a clock a test can step by hand, so idle
// arithmetic never depends on how long the test took to run.
func fixedClock(at *time.Time) func() time.Time {
	return func() time.Time { return *at }
}

// newIdleState builds a detector with no supervisor behind it, for the
// tests that only drive the marker stream. Close would panic on one of
// these, since nothing was started to cancel.
func newIdleState(timeout time.Duration, now func() time.Time) *SwayIdleDetector {
	return &SwayIdleDetector{
		timeout: timeout,
		logger:  testLogger(),
		now:     now,
	}
}

func idleTime(t *testing.T, d *SwayIdleDetector) time.Duration {
	t.Helper()

	idle, err := d.IdleTime(context.Background())
	if err != nil {
		t.Fatalf("IdleTime: %v", err)
	}
	return idle
}

func TestSwayIdleMarkers(t *testing.T) {
	t.Parallel()

	clock := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	d := newIdleState(5*time.Minute, fixedClock(&clock))

	if got := idleTime(t, d); got != 0 {
		t.Errorf("idle before any marker = %v, want 0", got)
	}

	// swayidle fires the timeout once the threshold has already
	// passed, so the reported idle time starts at the threshold rather
	// than at zero. That is what backdates the tracker's endedAt to
	// the last moment of activity.
	d.consume(strings.NewReader(swayIdleMarkerIdle + "\n"))
	if got := idleTime(t, d); got != 5*time.Minute {
		t.Errorf("idle at the event = %v, want 5m", got)
	}

	clock = clock.Add(30 * time.Second)
	if got := idleTime(t, d); got != 5*time.Minute+30*time.Second {
		t.Errorf("idle 30s later = %v, want 5m30s", got)
	}

	d.consume(strings.NewReader(swayIdleMarkerResume + "\n"))
	if got := idleTime(t, d); got != 0 {
		t.Errorf("idle after resume = %v, want 0", got)
	}
}

func TestSwayIdleIgnoresNoise(t *testing.T) {
	t.Parallel()

	clock := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	d := newIdleState(time.Minute, fixedClock(&clock))

	d.consume(strings.NewReader("starting\nsomething else\n"))
	if got := idleTime(t, d); got != 0 {
		t.Errorf("idle after unrecognised lines = %v, want 0", got)
	}

	// A stream that stops mid-line leaves whatever state the last
	// complete marker set.
	d.consume(strings.NewReader(swayIdleMarkerIdle + "\ntrackkr-res"))
	if got := idleTime(t, d); got != time.Minute {
		t.Errorf("idle after a truncated stream = %v, want 1m", got)
	}
}

func TestEffectiveTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		threshold time.Duration
		want      time.Duration
		wantErr   bool
	}{
		{name: "whole minutes", threshold: 5 * time.Minute, want: 5 * time.Minute},
		{name: "one second", threshold: time.Second, want: time.Second},
		// Rounding down would put the effective timeout below the
		// tracker's own threshold test, so it would ignore every event
		// swayidle sent and never record an idle period.
		{name: "rounds up", threshold: 1500 * time.Millisecond, want: 2 * time.Second},
		{name: "sub-second floors at one", threshold: 500 * time.Millisecond, want: time.Second},
		{name: "the bound itself", threshold: swayIdleMaxTimeoutSeconds * time.Second, want: swayIdleMaxTimeoutSeconds * time.Second},
		{name: "a nanosecond past the bound", threshold: swayIdleMaxTimeoutSeconds*time.Second + 1, wantErr: true},
		// A ceiling that wrapped would turn this into a small positive
		// number and pass the bound on the way through.
		{name: "the largest duration there is", threshold: math.MaxInt64, wantErr: true},
		{name: "zero", threshold: 0, wantErr: true},
		{name: "a negative threshold", threshold: -time.Second, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := effectiveTimeout(tt.threshold)
			if (err != nil) != tt.wantErr {
				t.Fatalf("effectiveTimeout(%v) error = %v, wantErr %v", tt.threshold, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("effectiveTimeout(%v) = %v, want %v", tt.threshold, got, tt.want)
			}
		})
	}
}

func TestSwayIdleCommandArgs(t *testing.T) {
	t.Parallel()

	cmd := swayIdleCommand(context.Background(), "/usr/bin/swayidle", 90*time.Second)
	args := cmd.Args[1:]

	// -C /dev/null keeps the daemon's swayidle from inheriting the
	// user's own config, whose commands lock the screen and suspend
	// the machine. Nothing else in the suite would catch its loss.
	if idx := slices.Index(args, "-C"); idx < 0 || idx+1 >= len(args) || args[idx+1] != os.DevNull {
		t.Errorf("args = %v, want -C %s", args, os.DevNull)
	}

	// -w serialises the two marker commands. Without it they are
	// written by unrelated processes and can arrive reversed.
	if !slices.Contains(args, "-w") {
		t.Errorf("args = %v, want -w", args)
	}

	want := []string{
		"timeout", "90", "echo " + swayIdleMarkerIdle,
		"resume", "echo " + swayIdleMarkerResume,
	}
	if !slices.Equal(args[len(args)-len(want):], want) {
		t.Errorf("args = %v, want them to end with %v", args, want)
	}
}

// scriptCmd runs a shell script in place of swayidle, so the
// supervisor is exercised for real on a machine with none installed.
func scriptCmd(script string) func(context.Context, time.Duration) *exec.Cmd {
	return func(ctx context.Context, _ time.Duration) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
}

// waitFor polls until cond holds, so a supervisor test never depends
// on a fixed sleep being long enough.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSwayIdleSupervisorReadsMarkers(t *testing.T) {
	clearSessionEnv(t)

	d := startSwayIdleDetector(time.Minute, testLogger(),
		scriptCmd("echo "+swayIdleMarkerIdle+"; sleep 30"))
	t.Cleanup(d.Close)

	waitFor(t, "the idle marker", func() bool {
		return idleTime(t, d) > 0
	})
}

func TestSwayIdleSupervisorRestartsTheChild(t *testing.T) {
	clearSessionEnv(t)

	// The child goes idle and then dies, which is what a compositor
	// restart does to it. The resume marker that would have cleared
	// the state died with it, so the supervisor has to clear it
	// instead -- otherwise the daemon reports a stale idle forever.
	// The sleep is what makes the idle state observable at all: the
	// supervisor clears it the moment the child exits, so a script
	// that echoed and died immediately would race the assertion.
	d := startSwayIdleDetector(time.Minute, testLogger(),
		scriptCmd("echo "+swayIdleMarkerIdle+"; sleep 2; exit 1"))
	t.Cleanup(d.Close)

	waitFor(t, "the idle marker", func() bool {
		return idleTime(t, d) > 0
	})
	waitFor(t, "the idle state to clear on restart", func() bool {
		return idleTime(t, d) == 0
	})
}

func TestSwayIdleCloseStopsTheChild(t *testing.T) {
	clearSessionEnv(t)

	d := startSwayIdleDetector(time.Minute, testLogger(), scriptCmd("sleep 300"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Close()
		// A second Close must be safe: main.go closes on the way out
		// of the function and again after the tracker returns.
		d.Close()
	}()

	select {
	case <-done:
	case <-time.After(swayIdleStopGrace + 5*time.Second):
		t.Fatal("Close did not return")
	}

	if got := idleTime(t, d); got != 0 {
		t.Errorf("idle after Close = %v, want 0", got)
	}
}

func TestSwayIdleResolvesDisplay(t *testing.T) {
	clearSessionEnv(t)

	dir := t.TempDir()
	t.Setenv(envRuntimeDir, dir)
	t.Setenv(envWaylandDisplay, "wayland-stale")
	listenAt(t, filepath.Join(dir, "wayland-7"))

	d := newIdleState(time.Minute, time.Now)
	cmd := exec.CommandContext(context.Background(), "/bin/true")
	d.applyDisplay(context.Background(), cmd)

	if !slices.Contains(cmd.Env, "WAYLAND_DISPLAY=wayland-7") {
		t.Errorf("env does not carry the resolved display: %v", displayEntries(cmd.Env))
	}
	// A second entry would leave which value the child sees up to its
	// libc.
	if got := len(displayEntries(cmd.Env)); got != 1 {
		t.Errorf("found %d WAYLAND_DISPLAY entries, want 1", got)
	}
}

func TestSwayIdleKeepsInheritedDisplay(t *testing.T) {
	clearSessionEnv(t)

	// Nothing live to resolve, so the inherited value stands. That is
	// the right answer whenever the compositor reused its name.
	t.Setenv(envRuntimeDir, t.TempDir())
	t.Setenv(envWaylandDisplay, "wayland-1")

	d := newIdleState(time.Minute, time.Now)
	cmd := exec.CommandContext(context.Background(), "/bin/true")
	d.applyDisplay(context.Background(), cmd)

	if cmd.Env != nil {
		t.Errorf("env was rewritten with nothing to resolve: %v", displayEntries(cmd.Env))
	}
}

func displayEntries(env []string) []string {
	var found []string
	for _, entry := range env {
		if strings.HasPrefix(entry, "WAYLAND_DISPLAY=") {
			found = append(found, entry)
		}
	}
	return found
}

func TestReplaceEnv(t *testing.T) {
	t.Parallel()

	got := replaceEnv([]string{"A=1", "WAYLAND_DISPLAY=old", "B=2"}, "WAYLAND_DISPLAY", "new")
	want := []string{"A=1", "B=2", "WAYLAND_DISPLAY=new"}
	if !slices.Equal(got, want) {
		t.Errorf("replaceEnv() = %v, want %v", got, want)
	}
}

// TestSwayIdleUnavailableWhileRestarting covers the window between one
// swayidle exiting and its replacement starting. The detector reports
// zero idle there, which is indistinguishable from a present user, so
// it has to say it is not watching instead.
func TestSwayIdleUnavailableWhileRestarting(t *testing.T) {
	clearSessionEnv(t)

	d := startSwayIdleDetector(time.Minute, testLogger(),
		scriptCmd("sleep 2; exit 1"))
	t.Cleanup(d.Close)

	waitFor(t, "the child to start", d.IdleAvailable)

	// The child dies and the supervisor waits before replacing it.
	waitFor(t, "the restart gap", func() bool { return !d.IdleAvailable() })

	if idleUsable(d) {
		t.Error("a detector with no child reported usable")
	}
}

func TestSwayIdleAvailableWhileWatching(t *testing.T) {
	clearSessionEnv(t)

	d := startSwayIdleDetector(time.Minute, testLogger(), scriptCmd("sleep 300"))
	t.Cleanup(d.Close)

	waitFor(t, "the child to start", d.IdleAvailable)
	if !idleUsable(d) {
		t.Error("a running detector reported unusable")
	}

	d.Close()
	if d.IdleAvailable() {
		t.Error("a closed detector still reported available")
	}
}
