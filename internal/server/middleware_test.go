package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yelinaung/trackkr/internal/db"
)

// stubSessionQuerier implements only the lookup the auth gate needs.
type stubSessionQuerier struct {
	user *db.UserRow
	err  error
}

func (s stubSessionQuerier) GetUserByID(context.Context, int64) (*db.UserRow, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.user, nil
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestRequireSessionAllowsValidCookie(t *testing.T) {
	t.Parallel()
	codec := newSessionCodec(testSecret, true)
	user := &db.UserRow{ID: 42, Username: "ye"}

	var seen *db.UserRow
	handler := RequireSession(codec, stubSessionQuerier{user: user})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = UserFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}),
	)

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	//nolint:gosec // G124: request-side cookie in a test; response flags are asserted separately.
	r.AddCookie(&http.Cookie{
		Name:  sessionCookieName,
		Value: codec.encode(42, time.Now().Add(time.Hour)),
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seen == nil || seen.ID != 42 {
		t.Errorf("context user = %+v, want id 42", seen)
	}
}

func TestRequireSessionRejects(t *testing.T) {
	t.Parallel()
	codec := newSessionCodec(testSecret, true)

	tests := []struct {
		name    string
		cookie  string
		queries SessionQuerier
	}{
		{
			name:    "no cookie",
			queries: stubSessionQuerier{user: &db.UserRow{ID: 42}},
		},
		{
			name:    "expired cookie",
			cookie:  codec.encode(42, time.Now().Add(-time.Minute)),
			queries: stubSessionQuerier{user: &db.UserRow{ID: 42}},
		},
		{
			name:    "forged cookie",
			cookie:  "42.99999999999.bogus",
			queries: stubSessionQuerier{user: &db.UserRow{ID: 42}},
		},
		{
			// A user deleted while holding a valid cookie must not
			// stay signed in, and must not 500 either.
			name:    "deleted user",
			cookie:  codec.encode(42, time.Now().Add(time.Hour)),
			queries: stubSessionQuerier{err: pgx.ErrNoRows},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			handler := RequireSession(codec, tt.queries)(okHandler())

			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			if tt.cookie != "" {
				r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tt.cookie}) //nolint:gosec // G124: request-side cookie in a test; response flags are asserted separately.
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)

			if rec.Code != http.StatusSeeOther {
				t.Errorf("status = %d, want 303", rec.Code)
			}
			if loc := rec.Header().Get("Location"); loc != testLoginPath {
				t.Errorf("Location = %q, want /login", loc)
			}
		})
	}
}

// A database outage is not a logout. Clearing the cookie here would
// sign out every active user for the length of the outage, and they
// would have to log in again afterwards for no reason.
func TestRequireSessionKeepsCookieOnTransientError(t *testing.T) {
	t.Parallel()
	codec := newSessionCodec(testSecret, true)
	queries := stubSessionQuerier{err: errors.New("connection refused")}
	handler := RequireSession(codec, queries)(okHandler())

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{ //nolint:gosec // G124: request-side cookie in a test.
		Name:  sessionCookieName,
		Value: codec.encode(42, time.Now().Add(time.Hour)),
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge < 0 {
			t.Error("session cookie was cleared by a transient failure")
		}
	}
}

func TestAttemptLimiterReleaseReturnsAnAttempt(t *testing.T) {
	t.Parallel()
	now := time.Now()
	l := newAttemptLimiter(2, time.Minute)

	l.reserve(testLimiterIP, now)
	l.reserve(testLimiterIP, now)
	if l.reserve(testLimiterIP, now) {
		t.Fatal("expected throttling at the limit")
	}

	l.release(testLimiterIP)
	if !l.reserve(testLimiterIP, now) {
		t.Error("released attempt was not given back")
	}

	// Releasing more than was reserved must not underflow.
	l.release(testLimiterIP)
	l.release(testLimiterIP)
	l.release(testLimiterIP)
	if got := l.remaining(testLimiterIP, now); got != 2 {
		t.Errorf("remaining = %d, want 2", got)
	}
}

// An htmx request must get HX-Redirect, or the login page is swapped
// into whatever div triggered the request.
func TestRequireSessionUsesHXRedirectForHTMX(t *testing.T) {
	t.Parallel()
	handler := RequireSession(newSessionCodec(testSecret, true),
		stubSessionQuerier{user: &db.UserRow{ID: 1}})(okHandler())

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/timeline", nil)
	r.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)

	if got := rec.Header().Get("HX-Redirect"); got != testLoginPath {
		t.Errorf("HX-Redirect = %q, want /login", got)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		secureCookies bool
		wantHSTS      bool
	}{
		{name: "secure cookies", secureCookies: true, wantHSTS: true},
		{name: "local HTTP", secureCookies: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			SecurityHeaders(tt.secureCookies)(okHandler()).ServeHTTP(
				rec,
				httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil),
			)

			got := rec.Header().Get("Content-Security-Policy")
			for _, want := range []string{
				"default-src 'self'",
				"script-src 'self'",
				"style-src 'self'",
				"frame-ancestors 'none'",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("CSP %q missing %q", got, want)
				}
			}
			// No nonce anywhere: the design deliberately avoids inline styles.
			if strings.Contains(got, "nonce") || strings.Contains(got, "unsafe-inline") {
				t.Errorf("CSP should need neither nonce nor unsafe-inline: %q", got)
			}

			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Error("missing nosniff")
			}
			if rec.Header().Get("X-Frame-Options") != "DENY" {
				t.Error("missing X-Frame-Options")
			}
			if gotHSTS := rec.Header().Get("Strict-Transport-Security") != ""; gotHSTS != tt.wantHSTS {
				t.Errorf("HSTS present = %t, want %t", gotHSTS, tt.wantHSTS)
			}
		})
	}
}

func TestRequireCSRF(t *testing.T) {
	t.Parallel()
	codec := newSessionCodec(testSecret, true)
	handler := RequireCSRF(codec)(okHandler())

	t.Run("get passes without token", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/devices", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("post without token is forbidden", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/devices", nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("post with matching token passes", func(t *testing.T) {
		t.Parallel()
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/devices", nil)
		r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: testCSRFValue}) //nolint:gosec // G124: request-side cookie in a test; response flags are asserted separately.
		r.Header.Set(csrfHeaderName, testCSRFValue)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})
}

func TestAttemptLimiter(t *testing.T) {
	t.Parallel()
	now := time.Now()
	l := newAttemptLimiter(10, 15*time.Minute)

	for i := range 10 {
		if !l.reserve(testLimiterIP, now) {
			t.Fatalf("attempt %d blocked too early", i+1)
		}
	}
	if l.reserve(testLimiterIP, now) {
		t.Error("11th attempt allowed, want throttled")
	}

	// Another host is unaffected.
	if !l.reserve("10.0.0.2", now) {
		t.Error("unrelated host throttled")
	}

	// A success clears the bucket.
	l.reset(testLimiterIP)
	if !l.reserve(testLimiterIP, now) {
		t.Error("bucket not cleared after reset")
	}
}

// Checking and recording must be one atomic step. With a separate
// allow-then-record, a concurrent burst all passes the check before any
// of them records, so the limit is bypassed and every request still pays
// for a bcrypt comparison.
func TestAttemptLimiterIsAtomicUnderConcurrency(t *testing.T) {
	t.Parallel()
	const limit = 10
	now := time.Now()
	l := newAttemptLimiter(limit, 15*time.Minute)

	var granted atomic.Int64
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			if l.reserve(testLimiterIP, now) {
				granted.Add(1)
			}
		})
	}
	wg.Wait()

	if got := granted.Load(); got != limit {
		t.Errorf("granted = %d attempts, want exactly %d", got, limit)
	}
	if left := l.remaining(testLimiterIP, now); left != 0 {
		t.Errorf("remaining = %d, want 0", left)
	}
}

func TestAttemptLimiterWindowExpiresAndEvicts(t *testing.T) {
	t.Parallel()
	now := time.Now()
	l := newAttemptLimiter(2, time.Minute)

	l.reserve(testLimiterIP, now)
	l.reserve(testLimiterIP, now)
	if l.reserve(testLimiterIP, now) {
		t.Fatal("expected throttling at the limit")
	}

	later := now.Add(2 * time.Minute)
	if !l.reserve(testLimiterIP, later) {
		t.Error("window did not drain")
	}
	if len(l.attempts) != 1 {
		t.Errorf("stale hosts not evicted: %d entries", len(l.attempts))
	}
}

// RemoteAddr carries an ephemeral port; keying on it directly would give
// every request its own bucket and never throttle anything.
func TestClientHostStripsPort(t *testing.T) {
	t.Parallel()
	const peerAddr = "10.0.0.1:54321"

	tests := []struct{ addr, want string }{
		{peerAddr, testLimiterIP},
		{"10.0.0.1:65000", testLimiterIP},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"malformed", "malformed"},
	}

	for _, tt := range tests {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, testLoginPath, nil)
		r.RemoteAddr = tt.addr
		if got := clientHost(r, nil); got != tt.want {
			t.Errorf("clientHost(%q) = %q, want %q", tt.addr, got, tt.want)
		}
	}
}

func TestClientHostUsesForwardedForFromTrustedProxy(t *testing.T) {
	t.Parallel()
	trustedProxy := netip.MustParsePrefix("10.0.0.0/24")
	const peerAddr = "10.0.0.1:54321"

	tests := []struct {
		name       string
		remoteAddr string
		forwarded  string
		want       string
	}{
		{
			name:       "trusted peer uses the rightmost forwarded hop",
			remoteAddr: peerAddr,
			forwarded:  "198.51.100.1, 198.51.100.2",
			want:       "198.51.100.2",
		},
		{
			name:       "untrusted peer cannot spoof a client",
			remoteAddr: "10.0.1.1:54321",
			forwarded:  "198.51.100.2",
			want:       "10.0.1.1",
		},
		{
			name:       "invalid forwarded address falls back to peer",
			remoteAddr: peerAddr,
			forwarded:  "not-an-address",
			want:       testLimiterIP,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, testLoginPath, nil)
			r.RemoteAddr = tt.remoteAddr
			r.Header.Set("X-Forwarded-For", tt.forwarded)
			if got := clientHost(r, []netip.Prefix{trustedProxy}); got != tt.want {
				t.Errorf("clientHost() = %q, want %q", got, tt.want)
			}
		})
	}
}
