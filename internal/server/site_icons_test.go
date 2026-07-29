package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image/color"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/db"
	"github.com/yelinaung/trackkr/internal/icon"
)

func TestSiteIconRouteFetchesAndCachesForOneYear(t *testing.T) {
	t.Parallel()

	fake := newFakeWeb()
	user := fake.addUser(t, "site-icon-owner", testPassword)
	store := newMemorySiteIconStore()
	pngBytes := serverTestPNG(t, color.NRGBA{R: 0x1f, G: 0x6f, B: 0x5f, A: 0xff})
	fetcher := &fakeSiteFaviconFetcher{png: pngBytes}
	srv := siteIconWebServer(t, fake, store, fetcher)
	session, _ := signIn(t, srv, user.ID)

	path := signedSiteIconPath(srv, user.ID)
	rec := performSiteIconRequest(t, srv, session, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != siteIconSVGType {
		t.Errorf("initial Content-Type = %q, want %s", got, siteIconSVGType)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Cookie") {
		t.Errorf("Vary = %q, want Cookie", got)
	}
	store.waitForCompletion(t)

	cached := performSiteIconRequest(t, srv, session, path)
	if cached.Code != http.StatusOK || !bytes.Equal(cached.Body.Bytes(), pngBytes) {
		t.Errorf("cached response = %d, %d bytes", cached.Code, cached.Body.Len())
	}
	if fetcher.callCount() != 1 {
		t.Errorf("cached request fetched again; calls = %d", fetcher.callCount())
	}

	row, err := store.SiteIcon(t.Context(), user.ID, testSiteHost)
	if err != nil {
		t.Fatal(err)
	}
	if row.AttemptedAt == nil {
		t.Fatal("cache row lacks attempted_at")
	}
	wantExpiry := row.AttemptedAt.AddDate(1, 0, 0)
	if !row.ExpiresAt.Equal(wantExpiry) {
		t.Errorf("expiry = %s, want %s", row.ExpiresAt, wantExpiry)
	}
}

func TestSiteIconRouteNegativeCachesFailure(t *testing.T) {
	t.Parallel()

	fake := newFakeWeb()
	user := fake.addUser(t, "site-icon-fallback", testPassword)
	store := newMemorySiteIconStore()
	fetcher := &fakeSiteFaviconFetcher{
		png: serverTestPNG(t, color.NRGBA{R: 0xff, A: 0xff}),
		err: errors.New("no favicon"),
	}
	srv := siteIconWebServer(t, fake, store, fetcher)
	session, _ := signIn(t, srv, user.ID)

	path := signedSiteIconPath(srv, user.ID)
	rec := performSiteIconRequest(t, srv, session, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != siteIconSVGType {
		t.Errorf("Content-Type = %q, want %s", got, siteIconSVGType)
	}
	if !strings.Contains(rec.Body.String(), ">EX</text>") {
		t.Errorf("fallback lacks site monogram: %s", rec.Body.String())
	}
	store.waitForCompletion(t)

	second := performSiteIconRequest(t, srv, session, path)
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), ">EX</text>") {
		t.Errorf("negative-cache response = %d: %s", second.Code, second.Body.String())
	}
	if fetcher.callCount() != 1 {
		t.Errorf("negative cache fetched again; calls = %d", fetcher.callCount())
	}
	row, err := store.SiteIcon(t.Context(), user.ID, testSiteHost)
	if err != nil {
		t.Fatal(err)
	}
	if len(row.PNG) != 0 || len(row.SHA256) != 0 {
		t.Error("bytes returned with a fetch error were cached")
	}
}

func TestSiteIconRouteRepairsFreshCorruptIcon(t *testing.T) {
	t.Parallel()

	fake := newFakeWeb()
	user := fake.addUser(t, "site-icon-repair", testPassword)
	store := newMemorySiteIconStore()
	corruptPNG := serverTestPNG(t, color.NRGBA{R: 0xaa, A: 0xff})
	store.rows[siteIconStoreKey(user.ID, testSiteHost)] = &db.SiteIconRow{
		ID:        1,
		UserID:    user.ID,
		Site:      testSiteHost,
		PNG:       corruptPNG,
		SHA256:    make([]byte, sha256.Size),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	repairedPNG := serverTestPNG(t, color.NRGBA{G: 0xaa, A: 0xff})
	srv := siteIconWebServer(t, fake, store, &fakeSiteFaviconFetcher{png: repairedPNG})
	session, _ := signIn(t, srv, user.ID)
	path := signedSiteIconPath(srv, user.ID)

	initial := performSiteIconRequest(t, srv, session, path)
	if got := initial.Header().Get("Content-Type"); got != siteIconSVGType {
		t.Fatalf("initial Content-Type = %q, want fallback", got)
	}
	store.waitForCompletion(t)

	repaired := performSiteIconRequest(t, srv, session, path)
	if repaired.Code != http.StatusOK || !bytes.Equal(repaired.Body.Bytes(), repairedPNG) {
		t.Errorf("repaired response = %d, %d bytes", repaired.Code, repaired.Body.Len())
	}
}

func TestSiteIconRouteRejectsNonCanonicalAndIPKeys(t *testing.T) {
	t.Parallel()

	fake := newFakeWeb()
	user := fake.addUser(t, "site-icon-invalid", testPassword)
	fetcher := &fakeSiteFaviconFetcher{}
	srv := siteIconWebServer(t, fake, newMemorySiteIconStore(), fetcher)
	session, _ := signIn(t, srv, user.ID)

	for _, path := range []string{
		"/site-icons/EXAMPLE.com?sig=invalid",
		"/site-icons/127.0.0.1?sig=invalid",
		"/site-icons/localhost?sig=invalid",
		"/site-icons/example.com?sig=invalid",
	} {
		rec := performSiteIconRequest(t, srv, session, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", path, rec.Code)
		}
	}
	if fetcher.callCount() != 0 {
		t.Errorf("invalid keys caused %d fetches", fetcher.callCount())
	}
}

func TestSiteIconRouteDoesNotWaitForFetch(t *testing.T) {
	t.Parallel()

	fake := newFakeWeb()
	user := fake.addUser(t, "site-icon-async", testPassword)
	store := newMemorySiteIconStore()
	fetcher := newBlockingSiteFaviconFetcher()
	srv := siteIconWebServer(t, fake, store, fetcher)
	session, _ := signIn(t, srv, user.ID)

	recorder := performSiteIconRequest(
		t, srv, session, signedSiteIconPath(srv, user.ID),
	)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != siteIconSVGType {
		t.Errorf("initial response = %d %q, want immediate SVG fallback",
			recorder.Code, recorder.Header().Get("Content-Type"))
	}
	fetcher.waitForStart(t)
	close(fetcher.release)
	store.waitForCompletion(t)
}

func TestSiteIconRefresherBoundsConcurrencyAndPerUserPending(t *testing.T) {
	t.Parallel()

	store := newMemorySiteIconStore()
	fetcher := newBlockingSiteFaviconFetcher()
	logger := zerolog.Nop()
	config := defaultSiteIconRefresherConfig()
	config.workers = 2
	config.queueLimit = 8
	config.userPendingLimit = 2
	refresher := newSiteIconRefresher(store, fetcher, &logger, config)
	t.Cleanup(refresher.Close)

	if !refresher.Enqueue(1, "one.example") || !refresher.Enqueue(1, "two.example") {
		t.Fatal("two per-user jobs were not queued")
	}
	if refresher.Enqueue(1, "three.example") {
		t.Fatal("third per-user pending job was accepted")
	}
	if !refresher.Enqueue(2, "four.example") || !refresher.Enqueue(3, "five.example") {
		t.Fatal("jobs for other users were not queued")
	}

	fetcher.waitForStart(t)
	fetcher.waitForStart(t)
	select {
	case site := <-fetcher.started:
		t.Fatalf("third fetch %q started while both workers were blocked", site)
	default:
	}

	close(fetcher.release)
	for range 2 {
		fetcher.waitForStart(t)
	}
	for range 4 {
		store.waitForCompletion(t)
	}
}

func TestSiteIconRefresherDefersRateLimitedJob(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		store := newMemorySiteIconStore()
		fetcher := &fakeSiteFaviconFetcher{err: errors.New("no favicon")}
		var logs synchronizedBuffer
		logger := zerolog.New(&logs)
		config := defaultSiteIconRefresherConfig()
		config.workers = 1
		config.queueLimit = 4
		config.userPendingLimit = 4
		config.rateLimit = 1
		refresher := newSiteIconRefresher(store, fetcher, &logger, config)
		t.Cleanup(refresher.Close)

		if !refresher.Enqueue(1, "first.example") ||
			!refresher.Enqueue(1, "second.example") {
			t.Fatal("refresh jobs were not queued")
		}
		synctest.Wait()
		if got := fetcher.callCount(); got != 1 {
			t.Fatalf("fetches before retry = %d, want 1", got)
		}
		if !strings.Contains(logs.String(), "site favicon refresh rate limited") {
			t.Error("rate-limited refresh was not logged")
		}

		time.Sleep(siteIconRateWindow)
		synctest.Wait()
		if got := fetcher.callCount(); got != 2 {
			t.Errorf("fetches after retry = %d, want 2", got)
		}
	})
}

func TestSiteIconRefresherRefundsNoopClaim(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	store := newMemorySiteIconStore()
	store.rows[siteIconStoreKey(1, "fresh.example")] = &db.SiteIconRow{
		ID:        1,
		UserID:    1,
		Site:      "fresh.example",
		ExpiresAt: now.Add(time.Hour),
	}
	fetcher := &fakeSiteFaviconFetcher{err: errors.New("no favicon")}
	logger := zerolog.Nop()
	config := defaultSiteIconRefresherConfig()
	config.workers = 1
	config.queueLimit = 2
	config.userPendingLimit = 2
	config.rateLimit = 1
	config.now = func() time.Time { return now }
	refresher := newSiteIconRefresher(store, fetcher, &logger, config)
	t.Cleanup(refresher.Close)

	if !refresher.Enqueue(1, "fresh.example") ||
		!refresher.Enqueue(1, "uncached.example") {
		t.Fatal("refresh jobs were not queued")
	}
	store.waitForCompletion(t)
	if got := fetcher.callCount(); got != 1 {
		t.Errorf("fetches = %d, want only the successfully claimed site", got)
	}
}

func TestSiteIconClaimLeaseIncludesCompletionMargin(t *testing.T) {
	t.Parallel()

	minimum := 2*siteIconDatabaseBudget + siteIconFetchBudget
	if siteIconClaimLease <= minimum {
		t.Errorf("claim lease = %s, want more than %s", siteIconClaimLease, minimum)
	}
}

func signedSiteIconPath(srv *Server, userID int64) string {
	return "/site-icons/" + testSiteHost + "?sig=" + srv.codec.siteIconSignature(userID, testSiteHost)
}

func siteIconWebServer(
	t *testing.T,
	fake *fakeWeb,
	store siteIconStore,
	fetcher siteFaviconFetcher,
) *Server {
	t.Helper()
	srv := webServer(t, fake, false)
	srv.siteIcons = store
	config := defaultSiteIconRefresherConfig()
	config.workers = 1
	config.queueLimit = 4
	config.userPendingLimit = 4
	refresher := newSiteIconRefresher(store, fetcher, srv.logger, config)
	srv.siteRefresh = refresher
	srv.closeSiteRefresh = refresher.Close
	srv.router = newRouter(srv)
	t.Cleanup(srv.Close)
	return srv
}

func performSiteIconRequest(
	t *testing.T,
	srv *Server,
	session *http.Cookie,
	path string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := newRequest(t, http.MethodGet, path, nil)
	request.AddCookie(session)
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, request)
	return recorder
}

type fakeSiteFaviconFetcher struct {
	mu    sync.Mutex
	png   []byte
	err   error
	calls int
}

type blockingSiteFaviconFetcher struct {
	started chan string
	release chan struct{}
}

func newBlockingSiteFaviconFetcher() *blockingSiteFaviconFetcher {
	return &blockingSiteFaviconFetcher{
		started: make(chan string, siteIconWorkerCount+1),
		release: make(chan struct{}),
	}
}

func (f *blockingSiteFaviconFetcher) Fetch(ctx context.Context, site string) ([]byte, error) {
	f.started <- site
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.release:
		return nil, errors.New("fixture has no favicon")
	}
}

func (f *blockingSiteFaviconFetcher) waitForStart(t *testing.T) string {
	t.Helper()
	select {
	case site := <-f.started:
		return site
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for favicon fetch")
		return ""
	}
}

func (f *fakeSiteFaviconFetcher) Fetch(context.Context, string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return bytes.Clone(f.png), f.err
}

func (f *fakeSiteFaviconFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type memorySiteIconStore struct {
	mu        sync.Mutex
	rows      map[string]*db.SiteIconRow
	nextID    int64
	completed chan struct{}
}

func newMemorySiteIconStore() *memorySiteIconStore {
	return &memorySiteIconStore{
		rows:      make(map[string]*db.SiteIconRow),
		completed: make(chan struct{}, 8),
	}
}

func (s *memorySiteIconStore) waitForCompletion(t *testing.T) {
	t.Helper()
	select {
	case <-s.completed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for favicon refresh")
	}
}

func (s *memorySiteIconStore) SiteIcon(_ context.Context, userID int64, site string) (*db.SiteIconRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[siteIconStoreKey(userID, site)]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return cloneSiteIconRow(row), nil
}

func (s *memorySiteIconStore) ClaimSiteIconRefresh(
	_ context.Context,
	userID int64,
	site string,
	now, claimUntil time.Time,
	repair bool,
) (*db.SiteIconRow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := siteIconStoreKey(userID, site)
	row, ok := s.rows[key]
	if ok && (row.ClaimUntil != nil && row.ClaimUntil.After(now) ||
		!repair && row.ExpiresAt.After(now)) {
		return cloneSiteIconRow(row), false, nil
	}
	if !ok {
		s.nextID++
		row = &db.SiteIconRow{ID: s.nextID, UserID: userID, Site: site, ExpiresAt: now, CreatedAt: now}
		s.rows[key] = row
	}
	row.ClaimUntil = new(claimUntil)
	row.UpdatedAt = now
	return cloneSiteIconRow(row), true, nil
}

func (s *memorySiteIconStore) CompleteSiteIconRefresh(
	_ context.Context,
	userID int64,
	site string,
	pngBytes []byte,
	details *icon.Details,
	attemptedAt, expiresAt, claimUntil time.Time,
) (*db.SiteIconRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[siteIconStoreKey(userID, site)]
	if !ok || row.ClaimUntil == nil || !row.ClaimUntil.Equal(claimUntil) {
		return nil, pgx.ErrNoRows
	}
	if details != nil {
		row.PNG = bytes.Clone(pngBytes)
		row.SHA256 = bytes.Clone(details.Digest[:])
		row.Width = new(details.Width)
		row.Height = new(details.Height)
	}
	row.AttemptedAt = new(attemptedAt)
	row.ExpiresAt = expiresAt
	row.ClaimUntil = nil
	row.UpdatedAt = attemptedAt
	s.completed <- struct{}{}
	return cloneSiteIconRow(row), nil
}

func siteIconStoreKey(userID int64, site string) string {
	return fmt.Sprintf("%d:%s", userID, site)
}

func cloneSiteIconRow(row *db.SiteIconRow) *db.SiteIconRow {
	clone := *row
	clone.PNG = bytes.Clone(row.PNG)
	clone.SHA256 = bytes.Clone(row.SHA256)
	if row.Width != nil {
		clone.Width = new(*row.Width)
	}
	if row.Height != nil {
		clone.Height = new(*row.Height)
	}
	if row.AttemptedAt != nil {
		clone.AttemptedAt = new(*row.AttemptedAt)
	}
	if row.ClaimUntil != nil {
		clone.ClaimUntil = new(*row.ClaimUntil)
	}
	return &clone
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func TestSiteIconResponseETag(t *testing.T) {
	t.Parallel()

	pngBytes := serverTestPNG(t, color.NRGBA{B: 0xcc, A: 0xff})
	digest := sha256.Sum256(pngBytes)
	row := &db.SiteIconRow{
		Site:      testSiteHost,
		PNG:       pngBytes,
		SHA256:    digest[:],
		ExpiresAt: time.Now().Add(time.Hour),
	}
	handler := &webHandlers{}
	request := newRequest(t, http.MethodGet, "/site-icons/example.com", nil)
	request.Header.Set("If-None-Match", `"`+hex.EncodeToString(digest[:])+`"`)
	recorder := httptest.NewRecorder()
	handler.serveSiteIcon(recorder, request, row, time.Now())
	if recorder.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", recorder.Code)
	}
}

func TestSiteIconResponseRejectsDigestMismatch(t *testing.T) {
	t.Parallel()

	pngBytes := serverTestPNG(t, color.NRGBA{R: 0xcc, A: 0xff})
	row := &db.SiteIconRow{
		Site:      testSiteHost,
		PNG:       pngBytes,
		SHA256:    make([]byte, sha256.Size),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	handler := &webHandlers{}
	request := newRequest(t, http.MethodGet, "/site-icons/example.com", nil)
	recorder := httptest.NewRecorder()
	handler.serveSiteIcon(recorder, request, row, time.Now())

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != siteIconSVGType {
		t.Errorf("Content-Type = %q, want %s", got, siteIconSVGType)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "private, max-age=60" {
		t.Errorf("Cache-Control = %q, want short fallback cache", got)
	}
}
