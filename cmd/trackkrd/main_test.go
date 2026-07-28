package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/tracker"
)

// clearTrackkrEnv unsets every TRACKKR_* override so a test is not
// affected by the caller's environment. It uses t.Setenv, so callers
// must not be parallel.
const testFirefoxApp = "firefox"

func clearTrackkrEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TRACKKR_SERVER_URL", "")
	t.Setenv("TRACKKR_API_KEY", "")
	t.Setenv("TRACKKR_DEVICE_NAME", "")
	t.Setenv("TRACKKR_EXTENSION_TOKEN", "")
}

type fakeWindow struct{ info tracker.WindowInfo }

func (f fakeWindow) ActiveWindow(context.Context) (tracker.WindowInfo, error) {
	return f.info, nil
}

type fakeIdle struct{}

func (fakeIdle) IdleTime(context.Context) (time.Duration, error) { return 0, nil }

func fakeDetectors(info tracker.WindowInfo) detectors {
	return detectors{
		newWindow: func() (tracker.WindowDetector, error) { return fakeWindow{info: info}, nil },
		newIdle:   func(*zerolog.Logger) tracker.IdleDetector { return fakeIdle{} },
	}
}

// writeConfig writes a daemon config pointing at serverURL and
// returns its path. The data dir is per-test so pending.json never
// touches the real one. It clears the TRACKKR_* overrides, which
// otherwise beat the file: callers must therefore not be parallel,
// and t.Setenv panics loudly if one ever is.
func writeConfig(t *testing.T, serverURL, extra string) string {
	t.Helper()
	clearTrackkrEnv(t)
	dir := t.TempDir()
	content := fmt.Sprintf(`
server_url = %q
api_key = "test_key"
data_dir = %q
%s
`, serverURL, dir, extra)

	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunWithoutAPIKey(t *testing.T) {
	clearTrackkrEnv(t)

	logger := zerolog.Nop()
	err := run(
		context.Background(),
		&logger,
		filepath.Join(t.TempDir(), "absent.toml"),
		fakeDetectors(tracker.WindowInfo{}),
	)
	if err == nil {
		t.Fatal("expected error when no api_key is configured")
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Errorf("error = %v, want it to mention api_key", err)
	}
}

// The config file is parsed before any env var is consulted, so this
// test needs no t.Setenv and can run in parallel.
func TestRunWithInvalidConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("not = valid toml ["), 0o600); err != nil {
		t.Fatal(err)
	}

	logger := zerolog.Nop()
	err := run(context.Background(), &logger, path, fakeDetectors(tracker.WindowInfo{}))
	if err == nil {
		t.Fatal("expected error for malformed config file")
	}
	if !strings.Contains(err.Error(), "loading config") {
		t.Errorf("error = %v, want it to mention loading config", err)
	}
}

func TestRunWithoutWindowDetector(t *testing.T) {
	d := detectors{
		newWindow: func() (tracker.WindowDetector, error) {
			return nil, tracker.ErrNoActiveWindow
		},
		newIdle: func(*zerolog.Logger) tracker.IdleDetector { return fakeIdle{} },
	}

	logger := zerolog.Nop()
	err := run(context.Background(), &logger, writeConfig(t, "http://127.0.0.1:1", ""), d)
	if err == nil {
		t.Fatal("expected error when the window detector cannot be created")
	}
	if !strings.Contains(err.Error(), "window detection unavailable") {
		t.Errorf("error = %v, want it to mention window detection", err)
	}
}

// Cancelling the context must unwind the whole daemon: the tracker
// loop, the reporter goroutine, and the final flush.
func TestRunStopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	path := writeConfig(t, srv.URL, `poll_interval = "10ms"
flush_interval = "20ms"`)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	logger := zerolog.Nop()
	go func() {
		done <- run(ctx, &logger, path, fakeDetectors(tracker.WindowInfo{AppName: testFirefoxApp}))
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s of cancellation")
	}
}

// End-to-end: the daemon finalizes its in-flight record on shutdown
// and flushes it to the server with the API key attached. A record
// needs at least a second of wall time, since sub-second durations
// are discarded.
func TestRunReportsRecordOnShutdown(t *testing.T) {
	var posts atomic.Int64
	gotKey := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		select {
		case gotKey <- r.Header.Get("X-API-Key"):
		default:
		}
		if r.URL.Path != "/api/v1/activity" {
			t.Errorf("path = %q, want /api/v1/activity", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	path := writeConfig(t, srv.URL, `poll_interval = "10ms"
flush_interval = "50ms"`)

	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()

	logger := zerolog.Nop()
	if err := run(ctx, &logger, path, fakeDetectors(tracker.WindowInfo{AppName: testFirefoxApp})); err != nil {
		t.Fatalf("run: %v", err)
	}

	if posts.Load() == 0 {
		t.Fatal("server received no activity batches")
	}
	if key := <-gotKey; key != "test_key" {
		t.Errorf("X-API-Key = %q, want test_key", key)
	}
}

// The browser listener and window detection are independent sources: on
// a platform with no detector the extension can still report tabs, so
// the daemon must run rather than refuse to start.
func TestRunWithoutWindowDetectorButWithExtension(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	token, err := tracker.GenerateExtensionToken()
	if err != nil {
		t.Fatal(err)
	}

	path := writeConfig(t, srv.URL, fmt.Sprintf(`poll_interval = "10ms"
flush_interval = "20ms"
extension_enabled = true
extension_addr = "127.0.0.1:0"
extension_token = %q`, token))

	d := detectors{
		newWindow: func() (tracker.WindowDetector, error) {
			return nil, tracker.ErrNoActiveWindow
		},
		newIdle: func(*zerolog.Logger) tracker.IdleDetector { return fakeIdle{} },
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	logger := zerolog.Nop()
	go func() { done <- run(ctx, &logger, path, d) }()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s of cancellation")
	}
}

// A listener that cannot bind must stop the daemon rather than leave it
// running without the feature the config enabled.
func TestRunFailsWhenListenerCannotBind(t *testing.T) {
	// Occupy the port first, so the daemon's listener cannot have it.
	var lc net.ListenConfig
	blocker, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Close() }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	token, err := tracker.GenerateExtensionToken()
	if err != nil {
		t.Fatal(err)
	}

	path := writeConfig(t, srv.URL, fmt.Sprintf(`poll_interval = "10ms"
extension_enabled = true
extension_addr = %q
extension_token = %q`, blocker.Addr().String(), token))

	d := detectors{
		newWindow: func() (tracker.WindowDetector, error) {
			return fakeWindow{info: tracker.WindowInfo{AppName: testFirefoxApp}}, nil
		},
		newIdle: func(*zerolog.Logger) tracker.IdleDetector { return fakeIdle{} },
	}

	logger := zerolog.Nop()
	done := make(chan error, 1)
	go func() { done <- run(context.Background(), &logger, path, d) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run returned nil; a listener that cannot bind must fail")
		}
		if !strings.Contains(err.Error(), "listening on") {
			t.Errorf("error = %v, want it to name the bind failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return; the daemon kept going without its listener")
	}
}
