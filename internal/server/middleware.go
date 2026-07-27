package server

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/yelinaung/trackkr/internal/db"
)

const userContextKey contextKey = "user"

// csp is static because no page emits inline CSS. Bar geometry travels as
// SVG presentation attributes, and htmx-config suppresses the indicator
// style block htmx would otherwise inject, so no nonce is needed -- which
// is just as well, since a nonce cannot survive an htmx swap.
const csp = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// UserFromContext returns the signed-in user, or nil.
func UserFromContext(ctx context.Context) *db.UserRow {
	u, _ := ctx.Value(userContextKey).(*db.UserRow)
	return u
}

// SessionQuerier is the single lookup the auth gate needs.
type SessionQuerier interface {
	GetUserByID(ctx context.Context, id int64) (*db.UserRow, error)
}

// RequireSession resolves the session cookie to a user. The cookie only
// carries an ID, so the row is loaded on every request: a user deleted
// while holding a valid cookie must not stay signed in.
func RequireSession(codec *sessionCodec, queries SessionQuerier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(sessionCookieName)
			if err != nil {
				redirectToLogin(w, r)
				return
			}

			userID, err := codec.decode(cookie.Value, time.Now())
			if err != nil {
				codec.clearSession(w)
				redirectToLogin(w, r)
				return
			}

			user, err := queries.GetUserByID(r.Context(), userID)
			if err != nil {
				codec.clearSession(w)
				redirectToLogin(w, r)
				return
			}

			ctx := context.WithValue(r.Context(), userContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// redirectToLogin sends htmx an HX-Redirect header instead of a 302, so a
// partial swap does not paint a login page inside a div.
func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// SecurityHeaders sets the same policy on every response.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// RequireCSRF rejects state-changing requests without a matching token.
func RequireCSRF(codec *sessionCodec) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}

			if !codec.checkCSRF(r) {
				http.Error(w, "invalid CSRF token", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// attemptLimiter throttles failed logins per client host.
type attemptLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	limit    int
	window   time.Duration
}

func newAttemptLimiter(limit int, window time.Duration) *attemptLimiter {
	return &attemptLimiter{
		attempts: make(map[string][]time.Time),
		limit:    limit,
		window:   window,
	}
}

// allow reports whether host may attempt again, sweeping expired entries
// so the map cannot grow without bound.
func (l *attemptLimiter) allow(host string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	return len(l.attempts[host]) < l.limit
}

func (l *attemptLimiter) record(host string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	l.attempts[host] = append(l.attempts[host], now)
}

// reset clears a host's history after a successful login.
func (l *attemptLimiter) reset(host string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, host)
}

func (l *attemptLimiter) sweep(now time.Time) {
	cutoff := now.Add(-l.window)
	for host, times := range l.attempts {
		kept := times[:0]
		for _, at := range times {
			if at.After(cutoff) {
				kept = append(kept, at)
			}
		}
		if len(kept) == 0 {
			delete(l.attempts, host)
			continue
		}
		l.attempts[host] = kept
	}
}

// clientHost strips the ephemeral source port from RemoteAddr. Keying on
// the raw value would give every request its own bucket. X-Forwarded-For
// is deliberately ignored: chi's RealIP was dropped from this router as
// spoofable, and a header-derived key is the same bypass in disguise.
func clientHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
