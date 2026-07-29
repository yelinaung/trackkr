package tracker

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/icon"
)

const testMissingApp = "Missing"

func TestAppIconCachePositiveAndNegativeExpiry(t *testing.T) {
	t.Parallel()

	initial := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	var nowNanos atomic.Int64
	nowNanos.Store(initial.UnixNano())
	var loads atomic.Int32
	cache := newAppIconCache(
		func() time.Time { return time.Unix(0, nowNanos.Load()) },
		func(_ context.Context, app appInfo) *icon.App {
			loads.Add(1)
			if app.Name == testMissingApp {
				return nil
			}
			value := trackerTestIcon(t, icon.AppKey(app.Name), 1)
			return &value
		},
	)
	t.Cleanup(cache.Close)

	app := appInfo{Name: testFinder, PID: 10}
	if firstPositive := cache.iconForApp(t.Context(), app); firstPositive != nil {
		t.Fatal("positive cache miss returned an icon synchronously")
	}
	waitForAppIconCache(t, cache, app)
	if cache.iconForApp(t.Context(), app) == nil {
		t.Fatal("positive cache hit returned nil")
	}
	if got := loads.Load(); got != 1 {
		t.Errorf("positive loads = %d, want 1", got)
	}
	nowNanos.Add(int64(appIconPositiveLifetime))
	if cache.iconForApp(t.Context(), app) != nil {
		t.Fatal("expired positive entry returned an icon")
	}
	waitForAppIconCache(t, cache, app)
	if cache.iconForApp(t.Context(), app) == nil {
		t.Fatal("positive reload was not cached")
	}
	if got := loads.Load(); got != 2 {
		t.Errorf("positive loads after expiry = %d, want 2", got)
	}

	missing := appInfo{Name: testMissingApp, PID: 11}
	if cache.iconForApp(t.Context(), missing) != nil {
		t.Fatal("negative cache returned an icon")
	}
	waitForAppIconCache(t, cache, missing)
	if cache.iconForApp(t.Context(), missing) != nil {
		t.Fatal("negative cache hit returned an icon")
	}
	if got := loads.Load(); got != 3 {
		t.Errorf("negative loads = %d, want 3", got)
	}
	nowNanos.Add(int64(appIconNegativeLifetime))
	if cache.iconForApp(t.Context(), missing) != nil {
		t.Fatal("negative reload returned an icon")
	}
	waitForAppIconCache(t, cache, missing)
	if got := loads.Load(); got != 4 {
		t.Errorf("negative loads after expiry = %d, want 4", got)
	}
}

func TestAppIconCachePIDReuseAndImmutableResults(t *testing.T) {
	t.Parallel()

	var loads atomic.Int32
	cache := newAppIconCache(
		time.Now,
		func(_ context.Context, app appInfo) *icon.App {
			loads.Add(1)
			if app.Name == testMissingApp {
				return nil
			}
			value := trackerTestIcon(t, icon.AppKey(app.Name), 1)
			return &value
		},
	)
	t.Cleanup(cache.Close)

	finder := appInfo{Name: testFinder, PID: 42}
	if cache.iconForApp(t.Context(), finder) != nil {
		t.Fatal("first lookup returned an icon synchronously")
	}
	waitForAppIconCache(t, cache, finder)
	first := cache.iconForApp(t.Context(), finder)
	if first == nil {
		t.Fatal("cached Finder icon is nil")
	}
	first.PNG[0] = 0
	again := cache.iconForApp(t.Context(), finder)
	if again == nil || again.PNG[0] != 0x89 {
		t.Error("caller mutated the cached PNG")
	}
	safari := appInfo{Name: "Safari", PID: 42}
	if cache.iconForApp(t.Context(), safari) != nil {
		t.Fatal("positive PID reuse returned an icon synchronously")
	}
	waitForAppIconCache(t, cache, safari)
	if cache.iconForApp(t.Context(), safari) == nil {
		t.Fatal("positive PID reuse did not load the new app")
	}
	missing := appInfo{Name: testMissingApp, PID: 43}
	if cache.iconForApp(t.Context(), missing) != nil {
		t.Fatal("missing app returned an icon")
	}
	waitForAppIconCache(t, cache, missing)
	terminal := appInfo{Name: "Terminal", PID: 43}
	if cache.iconForApp(t.Context(), terminal) != nil {
		t.Fatal("negative PID reuse returned an icon synchronously")
	}
	waitForAppIconCache(t, cache, terminal)
	if cache.iconForApp(t.Context(), terminal) == nil {
		t.Fatal("negative PID reuse hid the new app")
	}
	if got := loads.Load(); got != 4 {
		t.Errorf("loads = %d, want 4", got)
	}
}

func TestAppIconCacheMissesDoNotWaitForNativeLoad(t *testing.T) {
	t.Parallel()

	value := trackerTestIcon(t, icon.AppKey(testFinder), 1)
	started := make(chan struct{})
	release := make(chan struct{})
	var loads atomic.Int32
	cache := newAppIconCache(
		time.Now,
		func(_ context.Context, _ appInfo) *icon.App {
			if loads.Add(1) == 1 {
				close(started)
			}
			<-release
			loaded := icon.Clone(value)
			return &loaded
		},
	)
	t.Cleanup(cache.Close)

	app := appInfo{Name: testFinder, PID: 42}
	if got := cache.iconForApp(t.Context(), app); got != nil {
		t.Fatal("initial cache miss returned an icon")
	}
	<-started

	start := make(chan struct{})
	var nilResults atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			<-start
			if cache.iconForApp(t.Context(), app) == nil {
				nilResults.Add(1)
			}
		})
	}
	close(start)
	waited := make(chan struct{})
	go func() {
		wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("cache hits waited for the blocked native load")
	}

	if got := loads.Load(); got != 1 {
		t.Errorf("native loads = %d, want 1", got)
	}
	if got := nilResults.Load(); got != 20 {
		t.Errorf("non-blocking nil results = %d, want 20", got)
	}
	close(release)
	waitForAppIconCache(t, cache, app)
	if cache.iconForApp(t.Context(), app) == nil {
		t.Fatal("completed native load was not cached")
	}
}

func TestAppIconCacheCloseDoesNotWaitForNativeLoad(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	cache := newAppIconCache(
		time.Now,
		func(_ context.Context, _ appInfo) *icon.App {
			close(started)
			<-release
			return nil
		},
	)

	cache.iconForApp(t.Context(), appInfo{Name: testFinder, PID: 42})
	<-started
	closed := make(chan struct{})
	go func() {
		cache.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("cache close waited for the blocked native load")
	}
	close(release)
}

func TestAppIconCacheBoundsPendingNativeLoads(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	cache := newAppIconCache(
		time.Now,
		func(_ context.Context, _ appInfo) *icon.App {
			select {
			case <-started:
			default:
				close(started)
			}
			<-release
			return nil
		},
	)
	t.Cleanup(cache.Close)

	cache.iconForApp(t.Context(), appInfo{Name: "active-load", PID: 1})
	<-started
	for i := range appIconWorkerQueueLimit + 20 {
		cache.iconForApp(t.Context(), appInfo{
			Name: fmt.Sprintf("queued-%03d", i),
			PID:  i + 2,
		})
	}
	cache.mu.Lock()
	inFlight := len(cache.inFlight)
	cache.mu.Unlock()
	if inFlight > appIconWorkerQueueLimit+1 {
		t.Errorf("in-flight native loads = %d, want at most %d", inFlight, appIconWorkerQueueLimit+1)
	}
	if queued := len(cache.requests); queued != appIconWorkerQueueLimit {
		t.Errorf("queued native loads = %d, want %d", queued, appIconWorkerQueueLimit)
	}
	close(release)
}

func TestAppIconCacheRejectsInvalidAndBoundsEntries(t *testing.T) {
	t.Parallel()

	cache := newAppIconCache(
		time.Now,
		func(_ context.Context, app appInfo) *icon.App {
			if app.Name == "Invalid" {
				return &icon.App{Key: "invalid", PNG: []byte("bad")}
			}
			value := trackerTestIcon(t, icon.AppKey(app.Name), 1)
			return &value
		},
	)
	t.Cleanup(cache.Close)
	invalid := appInfo{Name: "Invalid", PID: 1}
	if cache.iconForApp(t.Context(), invalid) != nil {
		t.Error("invalid native PNG was accepted")
	}
	waitForAppIconCache(t, cache, invalid)
	for i := range appIconCacheLimit + 10 {
		app := appInfo{Name: "app-" + string(rune(0x100+i)), PID: i + 2}
		cache.iconForApp(t.Context(), app)
		waitForAppIconCache(t, cache, app)
	}
	cache.mu.Lock()
	entryCount := len(cache.entries)
	cache.mu.Unlock()
	if entryCount != appIconCacheLimit {
		t.Errorf("cache entries = %d, want %d", entryCount, appIconCacheLimit)
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if cache.iconForApp(cancelled, appInfo{Name: testFinder, PID: 999}) != nil {
		t.Error("cancelled lookup returned an icon")
	}
}

func TestAppIconForObservedProcessRejectsPIDReuse(t *testing.T) {
	t.Parallel()

	pngBytes := trackerTestIcon(t, "finder", 1).PNG
	got := appIconForObservedProcess(" Finder ", "finder", pngBytes)
	if got == nil || got.Key != "finder" {
		t.Fatalf("matching observed process icon = %#v", got)
	}
	pngBytes[0] = 0
	if got.PNG[0] != 0x89 {
		t.Error("observed process icon aliases native bytes")
	}
	if mismatch := appIconForObservedProcess("Finder", "Safari", got.PNG); mismatch != nil {
		t.Errorf("PID reuse icon = %#v, want nil", mismatch)
	}
}

func waitForAppIconCache(t *testing.T, cache *appIconCache, app appInfo) {
	t.Helper()

	key := appIconCacheKey{PID: app.PID, Key: icon.AppKey(app.Name)}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		cache.mu.Lock()
		_, loading := cache.inFlight[key]
		_, cached := cache.entries[key]
		cache.mu.Unlock()
		if !loading && cached {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("waiting for app icon cache entry for %#v", app)
		default:
			runtime.Gosched()
		}
	}
}

func TestTrackerAppIconDigestAndRefresh(t *testing.T) {
	t.Parallel()

	cfg := testReporterConfig(t)
	cfg.ServerURL = testUnusedServerURL
	logger := zerolog.Nop()
	reporter := NewReporter(cfg, http.DefaultClient, &logger)
	tracker := NewTracker(cfg, nil, nil, reporter, &logger)
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)

	first := trackerTestIcon(t, "finder", 1)
	tracker.maybeEnqueueAppIcon(WindowInfo{AppIcon: &first}, now)
	if reporter.AppIconQueueLen() != 1 {
		t.Fatalf("queue len = %d, want 1", reporter.AppIconQueueLen())
	}

	reporter.icons = make(map[string]icon.App)
	tracker.maybeEnqueueAppIcon(WindowInfo{AppIcon: &first}, now.Add(appIconRefreshInterval-time.Second))
	if reporter.AppIconQueueLen() != 0 {
		t.Error("unchanged icon was queued before refresh interval")
	}
	tracker.maybeEnqueueAppIcon(WindowInfo{AppIcon: &first}, now.Add(appIconRefreshInterval))
	if reporter.AppIconQueueLen() != 1 {
		t.Error("unchanged icon was not queued at refresh interval")
	}

	reporter.icons = make(map[string]icon.App)
	changed := trackerTestIcon(t, "finder", 2)
	tracker.maybeEnqueueAppIcon(WindowInfo{AppIcon: &changed}, now.Add(time.Minute))
	if reporter.AppIconQueueLen() != 1 {
		t.Error("changed digest was not queued immediately")
	}
}

func TestTrackerAppIconStateDoesNotAdvanceOnQueueRejection(t *testing.T) {
	t.Parallel()

	cfg := testReporterConfig(t)
	cfg.ServerURL = testUnusedServerURL
	logger := zerolog.Nop()
	reporter := NewReporter(cfg, http.DefaultClient, &logger)
	tracker := NewTracker(cfg, nil, nil, reporter, &logger)
	for i := range appIconQueueLimit {
		app := trackerTestIcon(t, fmt.Sprintf("queued-%03d", i), 1)
		if !reporter.EnqueueAppIcon(app) {
			t.Fatalf("filling reporter queue at %d", i)
		}
	}

	rejected := trackerTestIcon(t, "finder", 1)
	tracker.maybeEnqueueAppIcon(WindowInfo{AppIcon: &rejected}, time.Now())
	if _, exists := tracker.appIcons[rejected.Key]; exists {
		t.Error("tracker remembered an icon rejected by the reporter")
	}
}

func TestTrackerAppIconStateIsBounded(t *testing.T) {
	t.Parallel()

	cfg := testReporterConfig(t)
	cfg.ServerURL = testUnusedServerURL
	logger := zerolog.Nop()
	reporter := NewReporter(cfg, http.DefaultClient, &logger)
	tracker := NewTracker(cfg, nil, nil, reporter, &logger)
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	for i := range appIconStateLimit {
		key := "state-" + string(rune(0x100+i))
		tracker.appIcons[key] = queuedAppIcon{LastQueuedAt: now.Add(time.Duration(i) * time.Second)}
	}

	app := trackerTestIcon(t, "new-app", 1)
	tracker.maybeEnqueueAppIcon(WindowInfo{AppIcon: &app}, now.Add(time.Hour))
	if got := len(tracker.appIcons); got != appIconStateLimit {
		t.Errorf("tracker states = %d, want %d", got, appIconStateLimit)
	}
	if _, exists := tracker.appIcons["state-"+string(rune(0x100))]; exists {
		t.Error("oldest tracker state was not evicted")
	}
}

func trackerTestIcon(tb testing.TB, key string, shade byte) icon.App {
	tb.Helper()

	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for y := range 2 {
		for x := range 2 {
			img.SetNRGBA(x, y, color.NRGBA{R: shade, G: 0x55, B: 0xaa, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		tb.Fatalf("png.Encode: %v", err)
	}
	return icon.App{Key: key, PNG: buf.Bytes()}
}
