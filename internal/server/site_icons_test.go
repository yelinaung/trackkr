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
	"time"

	"github.com/jackc/pgx/v5"
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

	path := signedSiteIconPath(srv, user.ID, testSiteHost)
	rec := performSiteIconRequest(t, srv, session, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), pngBytes) {
		t.Error("response body does not match fetched PNG")
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Cookie") {
		t.Errorf("Vary = %q, want Cookie", got)
	}
	if fetcher.calls != 1 {
		t.Errorf("fetch calls = %d, want 1", fetcher.calls)
	}

	cached := performSiteIconRequest(t, srv, session, path)
	if cached.Code != http.StatusOK || !bytes.Equal(cached.Body.Bytes(), pngBytes) {
		t.Errorf("cached response = %d, %d bytes", cached.Code, cached.Body.Len())
	}
	if fetcher.calls != 1 {
		t.Errorf("cached request fetched again; calls = %d", fetcher.calls)
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
	fetcher := &fakeSiteFaviconFetcher{err: errors.New("no favicon")}
	srv := siteIconWebServer(t, fake, store, fetcher)
	session, _ := signIn(t, srv, user.ID)

	path := signedSiteIconPath(srv, user.ID, testSiteHost)
	rec := performSiteIconRequest(t, srv, session, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/svg+xml" {
		t.Errorf("Content-Type = %q, want image/svg+xml", got)
	}
	if !strings.Contains(rec.Body.String(), ">EX</text>") {
		t.Errorf("fallback lacks site monogram: %s", rec.Body.String())
	}
	if fetcher.calls != 1 {
		t.Errorf("fetch calls = %d, want 1", fetcher.calls)
	}

	second := performSiteIconRequest(t, srv, session, path)
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), ">EX</text>") {
		t.Errorf("negative-cache response = %d: %s", second.Code, second.Body.String())
	}
	if fetcher.calls != 1 {
		t.Errorf("negative cache fetched again; calls = %d", fetcher.calls)
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
	if fetcher.calls != 0 {
		t.Errorf("invalid keys caused %d fetches", fetcher.calls)
	}
}

func signedSiteIconPath(srv *Server, userID int64, site string) string {
	return "/site-icons/" + site + "?sig=" + srv.codec.siteIconSignature(userID, site)
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
	srv.favicons = fetcher
	srv.router = newRouter(srv)
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

func (f *fakeSiteFaviconFetcher) Fetch(context.Context, string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return bytes.Clone(f.png), f.err
}

type memorySiteIconStore struct {
	mu     sync.Mutex
	rows   map[string]*db.SiteIconRow
	nextID int64
}

func newMemorySiteIconStore() *memorySiteIconStore {
	return &memorySiteIconStore{rows: make(map[string]*db.SiteIconRow)}
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
) (*db.SiteIconRow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := siteIconStoreKey(userID, site)
	row, ok := s.rows[key]
	if ok && (row.ExpiresAt.After(now) || row.ClaimUntil != nil && row.ClaimUntil.After(now)) {
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
	if got := recorder.Header().Get("Content-Type"); got != "image/svg+xml" {
		t.Errorf("Content-Type = %q, want image/svg+xml", got)
	}
}
