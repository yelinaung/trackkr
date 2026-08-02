package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// fakeIdleDetector reports a fixed idle time, or an error. xprintidle can
// start failing long after startup, and the route has to say so rather
// than report the user as active.
type fakeIdleDetector struct {
	idle time.Duration
	err  error
}

func (f fakeIdleDetector) IdleTime(_ context.Context) (time.Duration, error) {
	return f.idle, f.err
}

func idleTestServer(t *testing.T, detector IdleDetector) *ExtensionServer {
	t.Helper()

	logger := zerolog.Nop()
	cfg := &Config{
		ExtensionAddr:  defaultExtensionAddr,
		ExtensionToken: testExtensionToken,
		IdleThreshold:  Duration{5 * time.Minute},
	}
	return NewExtensionServer(cfg, nil, &fakeEnqueuer{}, detector, &logger)
}

func idleRequest(t *testing.T, method, token string) *http.Request {
	t.Helper()

	r := httptest.NewRequestWithContext(
		context.Background(), method, "/extension/idle", nil,
	)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestExtensionIdleReportsWhenStopped(t *testing.T) {
	t.Parallel()

	// Six minutes idle against a five-minute threshold. The reply has to
	// carry the moment activity stopped, so the extension can close its
	// segment there however late it asks.
	srv := idleTestServer(t, fakeIdleDetector{idle: 6 * time.Minute})
	before := time.Now()

	w := httptest.NewRecorder()
	srv.handleIdle(w, idleRequest(t, http.MethodGet, testExtensionToken))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var got idleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding %q: %v", w.Body.String(), err)
	}
	if !got.Idle {
		t.Error("idle = false, want true")
	}
	if got.IdleSince == nil {
		t.Fatal("idle_since missing")
	}
	if got.ThresholdS != 300 {
		t.Errorf("threshold_s = %d, want 300", got.ThresholdS)
	}

	// idle_since is now minus the idle time, so it sits six minutes back.
	wantAround := before.Add(-6 * time.Minute)
	if delta := got.IdleSince.Sub(wantAround); delta < -time.Second || delta > time.Second {
		t.Errorf("idle_since = %v, want within a second of %v", got.IdleSince, wantAround)
	}
}

func TestExtensionIdleSilentBelowThreshold(t *testing.T) {
	t.Parallel()

	srv := idleTestServer(t, fakeIdleDetector{idle: time.Minute})

	w := httptest.NewRecorder()
	srv.handleIdle(w, idleRequest(t, http.MethodGet, testExtensionToken))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var got idleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding %q: %v", w.Body.String(), err)
	}
	if got.Idle {
		t.Error("idle = true one minute into a five-minute threshold")
	}
	if got.IdleSince != nil {
		t.Errorf("idle_since = %v, want it omitted", got.IdleSince)
	}
}

func TestExtensionIdleFailsLoudly(t *testing.T) {
	t.Parallel()

	// A broken detector must never read as "the user is active". The
	// extension would hold its segment open for as long as the detector
	// stayed broken, which is the overcounting this route exists to stop.
	srv := idleTestServer(t, fakeIdleDetector{err: errors.New("xprintidle died")})

	w := httptest.NewRecorder()
	srv.handleIdle(w, idleRequest(t, http.MethodGet, testExtensionToken))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if body := w.Body.String(); !json.Valid([]byte(body)) {
		t.Errorf("body %q is not JSON", body)
	}
}

func TestExtensionIdleRejectsUnauthorized(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		token  string
		want   int
	}{
		{"no token", http.MethodGet, "", http.StatusUnauthorized},
		{"wrong token", http.MethodGet, "not-the-token", http.StatusUnauthorized},
		{"post", http.MethodPost, testExtensionToken, http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := idleTestServer(t, fakeIdleDetector{idle: 6 * time.Minute})
			w := httptest.NewRecorder()
			srv.handleIdle(w, idleRequest(t, tt.method, tt.token))

			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}
