package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/db"
	"github.com/yelinaung/trackkr/internal/icon"
)

func TestAppIconUpload(t *testing.T) {
	t.Parallel()

	srv, mock := unitServer(t)
	fixture := createMockFixtures(t, mock)
	apps := []icon.App{
		{Key: testFinderLower, PNG: serverTestPNG(t, color.NRGBA{R: 0xaa, A: 0xff})},
		{Key: "safari", PNG: serverTestPNG(t, color.NRGBA{B: 0xaa, A: 0xff})},
	}
	rec := performIconUpload(t, srv, fixture.APIKey, apps)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var response appIconResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if response.Accepted != len(apps) {
		t.Errorf("accepted = %d, want %d", response.Accepted, len(apps))
	}
	if mock.iconUserID != fixture.Device.UserID {
		t.Errorf("user ID = %d, want %d", mock.iconUserID, fixture.Device.UserID)
	}
	if len(mock.icons) != len(apps) {
		t.Errorf("stored icons = %d, want %d", len(mock.icons), len(apps))
	}
}

func TestAppIconUploadValidation(t *testing.T) {
	t.Parallel()

	valid := icon.App{Key: testFinderLower, PNG: serverTestPNG(t, color.NRGBA{R: 0x44, A: 0xff})}
	tests := []struct {
		name        string
		contentType string
		body        []byte
		want        int
	}{
		{"content type", "text/plain", mustJSON(t, appIconRequest{Icons: []icon.App{valid}}), http.StatusUnsupportedMediaType},
		{"empty batch", jsonContentType, mustJSON(t, appIconRequest{}), http.StatusBadRequest},
		{"too many", jsonContentType, mustJSON(t, appIconRequest{Icons: repeatedIcons(valid, appIconBatchLimit+1)}), http.StatusBadRequest},
		{"duplicate", jsonContentType, mustJSON(t, appIconRequest{Icons: []icon.App{valid, valid}}), http.StatusBadRequest},
		{"invalid icon", jsonContentType, mustJSON(t, appIconRequest{Icons: []icon.App{{Key: testFinderLower, PNG: []byte("bad")}}}), http.StatusUnprocessableEntity},
		{"unknown field", jsonContentType, []byte(`{"icons":[],"surprise":true}`), http.StatusBadRequest},
		{"trailing value", jsonContentType, append(mustJSON(t, appIconRequest{Icons: []icon.App{valid}}), []byte(` {}`)...), http.StatusBadRequest},
		{"oversized", jsonContentType, bytes.Repeat([]byte(" "), appIconBodyLimit+1), http.StatusRequestEntityTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv, mock := unitServer(t)
			fixture := createMockFixtures(t, mock)
			req := newRequest(t, http.MethodPost, "/api/v1/app-icons", bytes.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			req.Header.Set("X-API-Key", fixture.APIKey)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d; body: %s", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}

func TestAppIconUploadInvalidRequestConsumesRate(t *testing.T) {
	t.Parallel()

	srv, mock := unitServer(t)
	fixture := createMockFixtures(t, mock)
	srv.iconLimit.limit = 1

	bad := newRequest(t, http.MethodPost, "/api/v1/app-icons", strings.NewReader("bad"))
	bad.Header.Set("Content-Type", jsonContentType)
	bad.Header.Set("X-API-Key", fixture.APIKey)
	srv.ServeHTTP(httptest.NewRecorder(), bad)

	valid := icon.App{Key: testFinderLower, PNG: serverTestPNG(t, color.NRGBA{A: 0xff})}
	rec := performIconUpload(t, srv, fixture.APIKey, []icon.App{valid})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("Retry-After is empty")
	}
}

func TestAppIconRateLimiterIsAtomicAndSweeps(t *testing.T) {
	t.Parallel()

	limiter := newAppIconRateLimiter(appIconRateLimit, appIconRateWindow)
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	start := make(chan struct{})
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range appIconRateLimit * 2 {
		wg.Go(func() {
			<-start
			if ok, _ := limiter.reserve(7, now); ok {
				allowed.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if got := allowed.Load(); got != appIconRateLimit {
		t.Errorf("allowed = %d, want %d", got, appIconRateLimit)
	}
	if ok, retry := limiter.reserve(7, now.Add(30*time.Minute)); ok || retry != 30*time.Minute {
		t.Errorf("reserve = %v, %v; want false, 30m", ok, retry)
	}

	limiter.hits[99] = []time.Time{now.Add(-2 * time.Hour)}
	limiter.lastSweep = now.Add(-2 * time.Hour)
	if ok, _ := limiter.reserve(8, now); !ok {
		t.Fatal("fresh device was rate limited")
	}
	if _, exists := limiter.hits[99]; exists {
		t.Error("expired device bucket was not swept")
	}
}

func TestAppIconUploadLogsEvictionWithoutKey(t *testing.T) {
	t.Parallel()

	srv, mock := unitServer(t)
	fixture := createMockFixtures(t, mock)
	mock.iconEvicted = 2
	var logs bytes.Buffer
	logger := zerolog.New(&logs)
	srv.logger = &logger
	srv.router = newRouter(srv)

	app := icon.App{Key: "private-app-name", PNG: serverTestPNG(t, color.NRGBA{A: 0xff})}
	rec := performIconUpload(t, srv, fixture.APIKey, []icon.App{app})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(logs.String(), app.Key) {
		t.Errorf("eviction log exposes app key: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"evicted":2`) {
		t.Errorf("eviction log lacks count: %s", logs.String())
	}
}

func TestAppIconImageRoute(t *testing.T) {
	t.Parallel()

	fake := newFakeWeb()
	user := fake.addUser(t, "icon-owner", testPassword)
	pngBytes := serverTestPNG(t, color.NRGBA{G: 0xbb, A: 0xff})
	digest := sha256.Sum256(pngBytes)
	fake.icons = []db.AppIconRow{{
		ID: 9, UserID: user.ID, AppKey: testFinderLower, PNG: pngBytes, SHA256: digest[:],
	}}
	srv := webServer(t, fake, false)
	session, _ := signIn(t, srv, user.ID)
	path := "/app-icons/9/" + hex.EncodeToString(digest[:]) + ".png"

	req := newRequest(t, http.MethodGet, path, nil)
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), pngBytes) {
		t.Error("response body does not match PNG")
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Cookie") {
		t.Errorf("Vary = %q, want Cookie", got)
	}
	etag := `"` + hex.EncodeToString(digest[:]) + `"`
	if got := rec.Header().Get("ETag"); got != etag {
		t.Errorf("ETag = %q, want %q", got, etag)
	}

	cached := newRequest(t, http.MethodGet, path, nil)
	cached.AddCookie(session)
	cached.Header.Set("If-None-Match", `"other", `+etag)
	cachedRec := httptest.NewRecorder()
	srv.ServeHTTP(cachedRec, cached)
	if cachedRec.Code != http.StatusNotModified {
		t.Errorf("cached status = %d, want 304", cachedRec.Code)
	}
	if got := cachedRec.Header().Get("Vary"); !strings.Contains(got, "Cookie") {
		t.Errorf("cached Vary = %q, want Cookie", got)
	}
}

func TestAppIconImageRouteHidesOtherUsersAndStaleDigests(t *testing.T) {
	t.Parallel()

	fake := newFakeWeb()
	owner := fake.addUser(t, "owner", testPassword)
	other := fake.addUser(t, "other", testPassword)
	pngBytes := serverTestPNG(t, color.NRGBA{R: 0x11, B: 0x99, A: 0xff})
	digest := sha256.Sum256(pngBytes)
	fake.icons = []db.AppIconRow{{
		ID: 12, UserID: owner.ID, AppKey: "safari", PNG: pngBytes, SHA256: digest[:],
	}}
	srv := webServer(t, fake, false)

	tests := []struct {
		name   string
		userID int64
		path   string
	}{
		{"other user", other.ID, "/app-icons/12/" + hex.EncodeToString(digest[:]) + ".png"},
		{"stale digest", owner.ID, "/app-icons/12/" + strings.Repeat("0", 64) + ".png"},
		{"uppercase digest", owner.ID, "/app-icons/12/" + strings.ToUpper(hex.EncodeToString(digest[:])) + ".png"},
		{"invalid id", owner.ID, "/app-icons/nope/" + hex.EncodeToString(digest[:]) + ".png"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			session, _ := signIn(t, srv, tt.userID)
			req := newRequest(t, http.MethodGet, tt.path, nil)
			req.AddCookie(session)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
		})
	}
}

func TestDashboardDecoratesAppTotalsWithIcons(t *testing.T) {
	t.Parallel()

	fake := newFakeWeb()
	user := fake.addUser(t, "dashboard-icons", testPassword)
	fake.totals = []db.AppTotalRow{{AppName: "Finder", Seconds: 60}}
	today := time.Now().UTC()
	start := time.Date(today.Year(), today.Month(), today.Day(), 9, 0, 0, 0, time.UTC)
	fake.devices = []db.DeviceRow{{ID: 3, UserID: user.ID, Name: testLaptop}}
	fake.records = []db.ActivityRecordRow{{
		DeviceID: 3, AppName: "Finder", StartedAt: start, EndedAt: start.Add(time.Minute),
	}}
	pngBytes := serverTestPNG(t, color.NRGBA{R: 0xee, A: 0xff})
	digest := sha256.Sum256(pngBytes)
	fake.icons = []db.AppIconRow{{
		ID: 44, UserID: user.ID, AppKey: testFinderLower, SHA256: digest[:],
	}}
	srv := webServer(t, fake, false)
	session, _ := signIn(t, srv, user.ID)

	req := newRequest(t, http.MethodGet, "/", nil)
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	want := "/app-icons/44/" + hex.EncodeToString(digest[:]) + ".png"
	if !strings.Contains(rec.Body.String(), `src="`+want+`"`) {
		t.Errorf("dashboard lacks icon URL %q", want)
	}
	if strings.Contains(rec.Body.String(), "totals__monogram") {
		t.Error("dashboard rendered a monogram for an app with an icon")
	}

	fake.iconErr = errors.New("icon metadata unavailable")
	fallbackReq := newRequest(t, http.MethodGet, "/", nil)
	fallbackReq.AddCookie(session)
	fallbackRec := httptest.NewRecorder()
	srv.ServeHTTP(fallbackRec, fallbackReq)
	if fallbackRec.Code != http.StatusOK {
		t.Fatalf("fallback status = %d, want 200", fallbackRec.Code)
	}
	if !strings.Contains(fallbackRec.Body.String(), "totals__monogram") {
		t.Error("metadata failure did not render the monogram fallback")
	}
}

func TestAppMonogram(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want string
	}{
		{"Visual Studio Code", "VI"},
		{"éclair 9", "ÉC"},
		{"123", "12"},
		{" - ", "?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := appMonogram(tt.name); got != tt.want {
				t.Errorf("appMonogram(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestAppIconUploadDatabaseFailure(t *testing.T) {
	t.Parallel()

	srv, mock := unitServer(t)
	fixture := createMockFixtures(t, mock)
	mock.iconErr = errors.New("database unavailable")
	app := icon.App{Key: testFinderLower, PNG: serverTestPNG(t, color.NRGBA{A: 0xff})}
	rec := performIconUpload(t, srv, fixture.APIKey, []icon.App{app})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func performIconUpload(t *testing.T, srv *Server, apiKey string, apps []icon.App) *httptest.ResponseRecorder {
	t.Helper()

	body := mustJSON(t, appIconRequest{Icons: apps})
	req := newRequest(t, http.MethodPost, "/api/v1/app-icons", bytes.NewReader(body))
	req.Header.Set("Content-Type", jsonContentType+"; charset=utf-8")
	req.Header.Set("X-API-Key", apiKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func mustJSON(tb testing.TB, value any) []byte {
	tb.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		tb.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func repeatedIcons(app icon.App, count int) []icon.App {
	apps := make([]icon.App, count)
	for i := range apps {
		apps[i] = app
		apps[i].Key += strings.Repeat("x", i)
	}
	return apps
}

func serverTestPNG(tb testing.TB, fill color.NRGBA) []byte {
	tb.Helper()

	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for y := range 2 {
		for x := range 2 {
			img.SetNRGBA(x, y, fill)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		tb.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}
