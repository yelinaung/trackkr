package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

const (
	//nolint:gosec // G101: a fixture, not a credential.
	testExtensionToken = "b8b0c1d2e3f405162738495a6b7c8d9e"
	testPageURL        = "https://example.com/a"
)

// fakeEnqueuer records what the listener would hand to the reporter.
type fakeEnqueuer struct {
	mu      sync.Mutex
	records []Record
}

func (f *fakeEnqueuer) Enqueue(rec *Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, *rec)
}

func (f *fakeEnqueuer) all() []Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Record(nil), f.records...)
}

func testExtensionServer(t *testing.T) (*ExtensionServer, *fakeEnqueuer) {
	t.Helper()
	logger := zerolog.Nop()
	queue := &fakeEnqueuer{}
	cfg := &Config{
		ExtensionAddr:  defaultExtensionAddr,
		ExtensionToken: testExtensionToken,
	}
	return NewExtensionServer(cfg, queue, &logger), queue
}

// activityRequest builds a well-formed request, so each test varies one
// thing from a known-good baseline.
func activityRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/extension/activity", strings.NewReader(body),
	)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+testExtensionToken)
	return r
}

func oneRecordBody(start, end time.Time, url string) string {
	return fmt.Sprintf(
		`{"records":[{"url":%q,"title":"a page","started_at":%q,"ended_at":%q}]}`,
		url, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano),
	)
}

func TestExtensionAcceptsValidRecord(t *testing.T) {
	t.Parallel()
	srv, queue := testExtensionServer(t)

	start := time.Now().Add(-90 * time.Second).UTC()
	end := start.Add(90 * time.Second)

	rec := httptest.NewRecorder()
	srv.server.Handler.ServeHTTP(rec, activityRequest(t, oneRecordBody(start, end, "https://example.com/docs")))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}

	got := queue.all()
	if len(got) != 1 {
		t.Fatalf("queued %d records, want 1", len(got))
	}
	if got[0].AppName != extensionAppName {
		t.Errorf("app_name = %q, want %q", got[0].AppName, extensionAppName)
	}
	if got[0].URL != "https://example.com/docs" {
		t.Errorf("url = %q, want the reported url", got[0].URL)
	}
	// The daemon computes the duration rather than trusting the caller.
	if got[0].DurationS != 90 {
		t.Errorf("duration = %d, want 90", got[0].DurationS)
	}
}

func TestExtensionRejectsBadRequests(t *testing.T) {
	t.Parallel()

	start := time.Now().Add(-time.Minute).UTC()
	body := oneRecordBody(start, start.Add(time.Minute), "https://example.com")

	tests := []struct {
		name     string
		mutate   func(*http.Request)
		wantCode int
	}{
		{
			name:     "wrong method",
			mutate:   func(r *http.Request) { r.Method = http.MethodGet },
			wantCode: http.StatusMethodNotAllowed,
		},
		{
			// A form post cannot set this content type, so requiring it
			// forces a preflight this server never answers.
			name:     "form content type",
			mutate:   func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
			wantCode: http.StatusUnsupportedMediaType,
		},
		{
			name:     "missing token",
			mutate:   func(r *http.Request) { r.Header.Del("Authorization") },
			wantCode: http.StatusUnauthorized,
		},
		{
			name:     "wrong token",
			mutate:   func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") },
			wantCode: http.StatusUnauthorized,
		},
		{
			// A page the user visits must not be able to write history.
			name:     "web origin",
			mutate:   func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
			wantCode: http.StatusForbidden,
		},
		{
			name:     "http localhost origin",
			mutate:   func(r *http.Request) { r.Header.Set("Origin", "http://localhost:3000") },
			wantCode: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv, queue := testExtensionServer(t)

			r := activityRequest(t, body)
			tt.mutate(r)
			rec := httptest.NewRecorder()
			srv.server.Handler.ServeHTTP(rec, r)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if n := len(queue.all()); n != 0 {
				t.Errorf("queued %d records, want none", n)
			}
		})
	}
}

// A charset parameter is normal and must not be rejected.
func TestExtensionAcceptsContentTypeWithCharset(t *testing.T) {
	t.Parallel()
	srv, _ := testExtensionServer(t)

	start := time.Now().Add(-time.Minute).UTC()
	r := activityRequest(t, oneRecordBody(start, start.Add(time.Minute), "https://example.com"))
	r.Header.Set("Content-Type", "application/json; charset=utf-8")

	rec := httptest.NewRecorder()
	srv.server.Handler.ServeHTTP(rec, r)

	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", rec.Code)
	}
}

// A moz-extension origin is the legitimate caller.
func TestExtensionAllowsExtensionOrigin(t *testing.T) {
	t.Parallel()
	srv, _ := testExtensionServer(t)

	start := time.Now().Add(-time.Minute).UTC()
	r := activityRequest(t, oneRecordBody(start, start.Add(time.Minute), "https://example.com"))
	r.Header.Set("Origin", "moz-extension://8f1b0c2d-3e4f-5061-7283-94a5b6c7d8e9")

	rec := httptest.NewRecorder()
	srv.server.Handler.ServeHTTP(rec, r)

	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
}

func TestExtensionFiltersRecords(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		url      string
		end      time.Time
		accepted bool
		wantSecs int
	}{
		{"ordinary page", testPageURL, start.Add(2 * time.Minute), true, 120},
		{"http page", "http://example.com/a", start.Add(time.Minute), true, 60},
		{"about page", "about:config", start.Add(time.Minute), false, 0},
		{"extension page", "moz-extension://abc/options.html", start.Add(time.Minute), false, 0},
		{"local file", "file:///home/ye/notes.txt", start.Add(time.Minute), false, 0},
		{"no host", "https://", start.Add(time.Minute), false, 0},
		{"sub-second flick", testPageURL, start.Add(300 * time.Millisecond), false, 0},
		{"zero length", testPageURL, start, false, 0},
		{"negative", testPageURL, start.Add(-time.Minute), false, 0},
		{
			// A suspended laptop wakes with one enormous span. Clamping
			// keeps the real browsing that preceded the sleep.
			name:     "suspended laptop is clamped, not dropped",
			url:      testPageURL,
			end:      start.Add(20 * time.Hour),
			accepted: true,
			wantSecs: int(maxRecordDuration.Seconds()),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv, queue := testExtensionServer(t)

			rec := httptest.NewRecorder()
			srv.server.Handler.ServeHTTP(rec, activityRequest(t, oneRecordBody(start, tt.end, tt.url)))

			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", rec.Code)
			}

			got := queue.all()
			if !tt.accepted {
				if len(got) != 0 {
					t.Fatalf("queued %+v, want nothing", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("queued %d records, want 1", len(got))
			}
			if got[0].DurationS != tt.wantSecs {
				t.Errorf("duration = %d, want %d", got[0].DurationS, tt.wantSecs)
			}
		})
	}
}

func TestExtensionRejectsEmptyAndMalformedBatches(t *testing.T) {
	t.Parallel()

	for _, body := range []string{`{"records":[]}`, `{}`, `not json`} {
		srv, _ := testExtensionServer(t)
		rec := httptest.NewRecorder()
		srv.server.Handler.ServeHTTP(rec, activityRequest(t, body))

		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
}

// A batch mirrors the server's ingest shape so draining a backlog after
// an outage costs one request.
func TestExtensionAcceptsBatch(t *testing.T) {
	t.Parallel()
	srv, queue := testExtensionServer(t)

	start := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	body := fmt.Sprintf(`{"records":[
		{"url":"https://a.example","title":"a","started_at":%q,"ended_at":%q},
		{"url":"about:blank","title":"skipped","started_at":%q,"ended_at":%q},
		{"url":"https://b.example","title":"b","started_at":%q,"ended_at":%q}
	]}`,
		start.Format(time.RFC3339), start.Add(time.Minute).Format(time.RFC3339),
		start.Format(time.RFC3339), start.Add(time.Minute).Format(time.RFC3339),
		start.Add(time.Minute).Format(time.RFC3339), start.Add(2*time.Minute).Format(time.RFC3339))

	rec := httptest.NewRecorder()
	srv.server.Handler.ServeHTTP(rec, activityRequest(t, body))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["accepted"] != 2 {
		t.Errorf("accepted = %d, want 2 (the about: URL is dropped)", resp["accepted"])
	}
	if n := len(queue.all()); n != 2 {
		t.Errorf("queued %d records, want 2", n)
	}
}

func TestExtensionStatus(t *testing.T) {
	t.Parallel()
	srv, _ := testExtensionServer(t)

	t.Run("with token", func(t *testing.T) {
		t.Parallel()
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/extension/status", nil)
		r.Header.Set("Authorization", "Bearer "+testExtensionToken)

		rec := httptest.NewRecorder()
		srv.server.Handler.ServeHTTP(rec, r)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"ok":true`) {
			t.Errorf("body = %q, want ok", rec.Body.String())
		}
	})

	// The popup distinguishes "daemon down" from "wrong token", so a bad
	// token must answer rather than hang up.
	t.Run("without token", func(t *testing.T) {
		t.Parallel()
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/extension/status", nil)

		rec := httptest.NewRecorder()
		srv.server.Handler.ServeHTTP(rec, r)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
}

// An empty configured token must never authenticate an empty header.
func TestExtensionRejectsEverythingWithoutAConfiguredToken(t *testing.T) {
	t.Parallel()
	logger := zerolog.Nop()
	srv := NewExtensionServer(&Config{ExtensionAddr: defaultExtensionAddr}, &fakeEnqueuer{}, &logger)

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/extension/status", nil)
	r.Header.Set("Authorization", "Bearer ")

	rec := httptest.NewRecorder()
	srv.server.Handler.ServeHTTP(rec, r)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestExtensionServerServeStopsOnCancel(t *testing.T) {
	t.Parallel()
	logger := zerolog.Nop()
	cfg := &Config{
		// Port 0 lets the OS choose, so parallel tests do not collide.
		ExtensionAddr:  "127.0.0.1:0",
		ExtensionToken: testExtensionToken,
	}
	srv := NewExtensionServer(cfg, &fakeEnqueuer{}, &logger)

	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if srv.Addr() == cfg.ExtensionAddr {
		t.Errorf("Addr = %q, want the port the OS actually assigned", srv.Addr())
	}

	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of cancellation")
	}
}

// A missing started_at decodes to year 1. The gap to a real ended_at
// saturates past the cap, so without an explicit check the record would
// be clamped into a plausible 12-hour session beginning in year 1 and
// inserted by a server that validates no timestamps of its own.
func TestExtensionRejectsZeroTimestamps(t *testing.T) {
	t.Parallel()

	end := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC).Format(time.RFC3339)

	bodies := map[string]string{
		"missing started_at": fmt.Sprintf(
			`{"records":[{"url":%q,"title":"t","ended_at":%q}]}`, testPageURL, end,
		),
		"missing ended_at": fmt.Sprintf(
			`{"records":[{"url":%q,"title":"t","started_at":%q}]}`, testPageURL, end,
		),
		"both missing": fmt.Sprintf(
			`{"records":[{"url":%q,"title":"t"}]}`, testPageURL,
		),
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, queue := testExtensionServer(t)

			rec := httptest.NewRecorder()
			srv.server.Handler.ServeHTTP(rec, activityRequest(t, body))

			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", rec.Code)
			}
			if got := queue.all(); len(got) != 0 {
				t.Errorf("queued %+v, want nothing", got)
			}
		})
	}
}

// Binding is what fails when the port is taken, and it must fail before
// anything starts serving.
func TestExtensionListenReportsBindFailure(t *testing.T) {
	t.Parallel()
	logger := zerolog.Nop()

	var lc net.ListenConfig
	blocker, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Close() }()

	cfg := &Config{
		ExtensionAddr:  blocker.Addr().String(),
		ExtensionToken: testExtensionToken,
	}
	srv := NewExtensionServer(cfg, &fakeEnqueuer{}, &logger)

	if err := srv.Listen(t.Context()); err == nil {
		t.Fatal("Listen succeeded on an occupied port")
	}
}

func TestExtensionServeWithoutListen(t *testing.T) {
	t.Parallel()
	logger := zerolog.Nop()
	srv := NewExtensionServer(&Config{ExtensionAddr: defaultExtensionAddr}, &fakeEnqueuer{}, &logger)

	if err := srv.Serve(t.Context()); err == nil {
		t.Error("Serve succeeded without a listener")
	}
}
