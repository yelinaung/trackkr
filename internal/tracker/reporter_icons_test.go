package tracker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/icon"
)

func TestEnqueueAppIconReplacesCopiesAndCaps(t *testing.T) {
	t.Parallel()

	cfg := testReporterConfig(t)
	cfg.ServerURL = testUnusedServerURL
	logger := zerolog.Nop()
	reporter := NewReporter(cfg, http.DefaultClient, &logger)

	first := trackerTestIcon(t, "finder", 0x11)
	if !reporter.EnqueueAppIcon(first) {
		t.Fatal("first EnqueueAppIcon returned false")
	}
	first.PNG[0] = 0
	if reporter.icons["finder"].PNG[0] != 0x89 {
		t.Error("reporter retained the caller's PNG slice")
	}
	replacement := trackerTestIcon(t, "finder", 0x22)
	if !reporter.EnqueueAppIcon(replacement) {
		t.Fatal("replacement EnqueueAppIcon returned false")
	}
	if reporter.AppIconQueueLen() != 1 {
		t.Errorf("queue len = %d, want 1", reporter.AppIconQueueLen())
	}

	for i := 1; i < appIconQueueLimit; i++ {
		app := trackerTestIcon(t, "app-"+strings.Repeat("x", i), 1)
		if !reporter.EnqueueAppIcon(app) {
			t.Fatalf("EnqueueAppIcon rejected key %d before capacity", i)
		}
	}
	if reporter.EnqueueAppIcon(trackerTestIcon(t, "overflow", 0xff)) {
		t.Error("EnqueueAppIcon accepted a new key above capacity")
	}
	if !reporter.EnqueueAppIcon(trackerTestIcon(t, "finder", 0x33)) {
		t.Error("EnqueueAppIcon rejected a replacement at capacity")
	}
	if reporter.EnqueueAppIcon(icon.App{Key: "invalid", PNG: []byte("bad")}) {
		t.Error("EnqueueAppIcon accepted invalid PNG")
	}
}

func TestReporterFlushesActivityBeforeAppIcons(t *testing.T) {
	t.Parallel()

	paths := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		if r.URL.Path == activityAPIPath {
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	reporter := newIconTestReporter(t, srv)
	reporter.Enqueue(testActivityRecord())
	reporter.EnqueueAppIcon(trackerTestIcon(t, "finder", 1))
	reporter.tryFlush(t.Context())

	first := <-paths
	second := <-paths
	if first != activityAPIPath || second != appIconAPIPath {
		t.Errorf("request order = %q, %q", first, second)
	}
}

func TestReporterFlushFailuresAreIndependent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		activityStatus int
		iconStatus     int
		wantActivity   int
		wantIcons      int
	}{
		{"icon failure", http.StatusCreated, http.StatusInternalServerError, 0, 1},
		{"activity failure", http.StatusInternalServerError, http.StatusOK, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == activityAPIPath {
					w.WriteHeader(tt.activityStatus)
					return
				}
				w.WriteHeader(tt.iconStatus)
			}))
			defer srv.Close()

			reporter := newIconTestReporter(t, srv)
			reporter.Enqueue(testActivityRecord())
			reporter.EnqueueAppIcon(trackerTestIcon(t, "finder", 1))
			reporter.tryFlush(t.Context())
			if got := reporter.QueueLen(); got != tt.wantActivity {
				t.Errorf("activity queue = %d, want %d", got, tt.wantActivity)
			}
			if got := reporter.AppIconQueueLen(); got != tt.wantIcons {
				t.Errorf("icon queue = %d, want %d", got, tt.wantIcons)
			}
		})
	}
}

func TestFlushAppIconsRemovesOnlyMatchingDigest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		replacement byte
		wantIcons   int
	}{
		{"identical replacement", 1, 0},
		{"changed replacement", 2, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			started := make(chan struct{})
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != appIconAPIPath {
					t.Errorf("path = %q", r.URL.Path)
				}
				close(started)
				<-release
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			reporter := newIconTestReporter(t, srv)
			reporter.EnqueueAppIcon(trackerTestIcon(t, "finder", 1))
			done := make(chan error, 1)
			go func() { done <- reporter.flushAppIcons(t.Context()) }()
			<-started
			if !reporter.EnqueueAppIcon(trackerTestIcon(t, "finder", tt.replacement)) {
				t.Fatal("replacement enqueue failed while request was blocked")
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatalf("flushAppIcons: %v", err)
			}
			if got := reporter.AppIconQueueLen(); got != tt.wantIcons {
				t.Errorf("queue len = %d, want %d", got, tt.wantIcons)
			}
		})
	}
}

func TestFlushAppIconsResponsePolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status int
		want   int
	}{
		{http.StatusBadRequest, 0},
		{http.StatusRequestEntityTooLarge, 0},
		{http.StatusUnprocessableEntity, 0},
		{http.StatusUnauthorized, 1},
		{http.StatusTooManyRequests, 1},
		{http.StatusInternalServerError, 1},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()
			reporter := newIconTestReporter(t, srv)
			reporter.EnqueueAppIcon(trackerTestIcon(t, "finder", 1))
			_ = reporter.flushAppIcons(t.Context())
			if got := reporter.AppIconQueueLen(); got != tt.want {
				t.Errorf("queue len = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFlushAppIconsIsolatesPermanentBatchRejection(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var upload appIconUploadRequest
		if err := json.NewDecoder(r.Body).Decode(&upload); err != nil {
			t.Errorf("decoding upload: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(upload.Icons) > 1 || upload.Icons[0].Key == "bad" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	reporter := newIconTestReporter(t, srv)
	for _, appIcon := range []icon.App{
		trackerTestIcon(t, "alpha", 1),
		trackerTestIcon(t, "bad", 2),
		trackerTestIcon(t, "charlie", 3),
	} {
		if !reporter.EnqueueAppIcon(appIcon) {
			t.Fatalf("enqueueing %q", appIcon.Key)
		}
	}
	if err := reporter.flushAppIcons(t.Context()); err == nil {
		t.Fatal("flushAppIcons returned nil for a permanently rejected icon")
	}
	if got := requests.Load(); got != 4 {
		t.Errorf("requests = %d, want one batch and three isolated requests", got)
	}
	if got := reporter.AppIconQueueLen(); got != 0 {
		t.Errorf("icon queue = %d, want accepted icons and the one poison icon resolved", got)
	}
}

func TestReporterAppIconRetryBackoff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		retryAfter string
		wantWait   time.Duration
	}{
		{"server error", http.StatusInternalServerError, "", appIconRetryMin},
		{"rate limit", http.StatusTooManyRequests, "600", 10 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if requests.Add(1) == 1 {
					if tt.retryAfter != "" {
						w.Header().Set("Retry-After", tt.retryAfter)
					}
					w.WriteHeader(tt.status)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
			reporter := newIconTestReporter(t, srv)
			reporter.now = func() time.Time { return now }
			reporter.EnqueueAppIcon(trackerTestIcon(t, "finder", 1))

			if err := reporter.flushAppIcons(t.Context()); err == nil {
				t.Fatal("first flush returned nil")
			}
			if wait := reporter.iconRetryAt.Sub(now); wait != tt.wantWait {
				t.Errorf("retry wait = %v, want %v", wait, tt.wantWait)
			}
			if err := reporter.flushAppIcons(t.Context()); err != nil {
				t.Fatalf("backed-off flush: %v", err)
			}
			if got := requests.Load(); got != 1 {
				t.Errorf("requests during backoff = %d, want 1", got)
			}

			now = now.Add(tt.wantWait)
			if err := reporter.flushAppIcons(t.Context()); err != nil {
				t.Fatalf("flush after backoff: %v", err)
			}
			if got := requests.Load(); got != 2 {
				t.Errorf("requests after backoff = %d, want 2", got)
			}
			if reporter.AppIconQueueLen() != 0 {
				t.Error("successful retry did not clear the icon")
			}
		})
	}
}

func TestReporterAppIconRetryBackoffGrows(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reporter := newIconTestReporter(t, srv)
	reporter.now = func() time.Time { return now }
	reporter.EnqueueAppIcon(trackerTestIcon(t, "finder", 1))

	if err := reporter.flushAppIcons(t.Context()); err == nil {
		t.Fatal("first flush returned nil")
	}
	now = now.Add(appIconRetryMin)
	if err := reporter.flushAppIcons(t.Context()); err == nil {
		t.Fatal("second flush returned nil")
	}
	if wait := reporter.iconRetryAt.Sub(now); wait != 2*appIconRetryMin {
		t.Errorf("second retry wait = %v, want %v", wait, 2*appIconRetryMin)
	}
	now = now.Add(appIconRetryMin)
	if err := reporter.flushAppIcons(t.Context()); err != nil {
		t.Fatalf("flush during second backoff: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"seconds", "120", 2 * time.Minute},
		{"HTTP date", now.Add(5 * time.Minute).Format(http.TimeFormat), 5 * time.Minute},
		{"negative", "-1", 0},
		{"malformed", "later", 0},
		{"bounded", "999999999", appIconRetryAfterMax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseRetryAfter(tt.value, now); got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestReporterAppIconTimeout(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		cfg := testReporterConfig(t)
		cfg.ServerURL = "http://blocked"
		logger := zerolog.Nop()
		reporter := NewReporter(cfg, blockingHTTPPoster{}, &logger)
		appIcon := trackerTestIcon(t, "finder", 1)
		reporter.EnqueueAppIcon(appIcon)
		body, err := json.Marshal(appIconUploadRequest{Icons: []icon.App{appIcon}})
		if err != nil {
			t.Fatalf("marshaling request: %v", err)
		}
		wantTimeout := appIconRequestTimeout(len(body))

		started := time.Now()
		reporter.tryFlush(t.Context())
		if elapsed := time.Since(started); elapsed != wantTimeout {
			t.Errorf("timeout elapsed = %v, want %v", elapsed, wantTimeout)
		}
		if reporter.AppIconQueueLen() != 1 {
			t.Error("timeout removed the pending icon")
		}
	})
}

func TestAppIconRequestTimeoutScalesWithBody(t *testing.T) {
	t.Parallel()

	small := appIconRequestTimeout(8 << 10)
	large := appIconRequestTimeout(900 << 10)
	capped := appIconRequestTimeout(2 << 20)
	if small <= appIconHTTPBaseTimeout {
		t.Errorf("small timeout = %v, want more than base %v", small, appIconHTTPBaseTimeout)
	}
	if large <= small {
		t.Errorf("large timeout = %v, want more than small timeout %v", large, small)
	}
	if capped != appIconHTTPMaxTimeout {
		t.Errorf("capped timeout = %v, want %v", capped, appIconHTTPMaxTimeout)
	}
}

func TestShutdownPersistsActivityBeforeAppIconRequest(t *testing.T) {
	t.Parallel()

	cfg := testReporterConfig(t)
	cfg.ServerURL = "http://ordered"
	logger := zerolog.Nop()
	poster := &shutdownOrderPoster{pending: filepath.Join(cfg.DataDir, "pending.json")}
	reporter := NewReporter(cfg, poster, &logger)
	reporter.Enqueue(testActivityRecord())
	reporter.EnqueueAppIcon(trackerTestIcon(t, "finder", 1))
	reporter.iconRetryAt = time.Now().Add(time.Hour)

	if err := reporter.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if poster.iconBeforePending {
		t.Error("application icon request started before pending activity was saved")
	}
}

func TestShutdownFlushesAppIconsWhenPendingSaveFails(t *testing.T) {
	t.Parallel()

	dataPath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(dataPath, []byte("occupied"), 0o600); err != nil {
		t.Fatalf("creating data-path blocker: %v", err)
	}
	cfg := testReporterConfig(t)
	cfg.DataDir = dataPath
	cfg.ServerURL = "http://shutdown"
	logger := zerolog.Nop()
	poster := &shutdownFailurePoster{}
	reporter := NewReporter(cfg, poster, &logger)
	reporter.Enqueue(testActivityRecord())
	reporter.EnqueueAppIcon(trackerTestIcon(t, "finder", 1))

	if err := reporter.Shutdown(); err == nil {
		t.Fatal("Shutdown returned nil after pending save failure")
	}
	if poster.iconRequests != 1 {
		t.Errorf("icon requests = %d, want 1 despite pending save failure", poster.iconRequests)
	}
}

func TestShutdownBoundsAppIconFlush(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		cfg := testReporterConfig(t)
		cfg.ServerURL = "http://blocked"
		logger := zerolog.Nop()
		reporter := NewReporter(cfg, blockingHTTPPoster{}, &logger)
		reporter.EnqueueAppIcon(trackerTestIcon(t, "finder", 1))

		started := time.Now()
		if err := reporter.Shutdown(); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		if elapsed := time.Since(started); elapsed != appIconShutdownTimeout {
			t.Errorf("shutdown icon timeout = %v, want %v", elapsed, appIconShutdownTimeout)
		}
	})
}

type blockingHTTPPoster struct{}

func (blockingHTTPPoster) Do(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

type shutdownOrderPoster struct {
	pending           string
	iconBeforePending bool
}

type shutdownFailurePoster struct {
	iconRequests int
}

func (p *shutdownFailurePoster) Do(req *http.Request) (*http.Response, error) {
	status := http.StatusInternalServerError
	if req.URL.Path == appIconAPIPath {
		p.iconRequests++
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       http.NoBody,
		Header:     make(http.Header),
	}, nil
}

func (p *shutdownOrderPoster) Do(req *http.Request) (*http.Response, error) {
	status := http.StatusInternalServerError
	if req.URL.Path == appIconAPIPath {
		status = http.StatusOK
		if _, err := os.Stat(p.pending); err != nil {
			p.iconBeforePending = true
		}
	}
	return &http.Response{
		StatusCode: status,
		Body:       http.NoBody,
		Header:     make(http.Header),
	}, nil
}

func newIconTestReporter(t *testing.T, srv *httptest.Server) *Reporter {
	t.Helper()
	cfg := testReporterConfig(t)
	cfg.ServerURL = srv.URL
	logger := zerolog.Nop()
	return NewReporter(cfg, srv.Client(), &logger)
}

func testActivityRecord() *Record {
	now := time.Now()
	return &Record{
		AppName: testFinder, StartedAt: now, EndedAt: now.Add(time.Second), DurationS: 1,
	}
}
