package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yelinaung/trackkr/internal/db"
)

const userContextKey contextKey = "user"

const (
	htmxRequestHeader = "HX-Request"
	htmxRequestValue  = "true"
)

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
				// Only a genuinely missing row means the session is
				// dead. Treating a database outage or a cancelled
				// request the same way would sign everyone out for the
				// duration of the outage.
				if !errors.Is(err, pgx.ErrNoRows) {
					http.Error(w, "service unavailable", http.StatusServiceUnavailable)
					return
				}
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
	if r.Header.Get(htmxRequestHeader) == htmxRequestValue {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// SecurityHeaders sets the same policy on every response.
func SecurityHeaders(secureCookies bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", csp)
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "same-origin")
			h.Set("X-Frame-Options", "DENY")
			if secureCookies {
				h.Set("Strict-Transport-Security", "max-age=31536000")
			}
			next.ServeHTTP(w, r)
		})
	}
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

// reserve claims one attempt for host, reporting false when the host is
// already at the limit.
//
// Checking and recording must happen under one lock. Splitting them lets
// a burst of concurrent requests all pass the check before any of them
// records, so an attacker gets an unbounded number of bcrypt comparisons
// out of a ten-attempt limit -- both a bypass and a CPU amplifier.
func (l *attemptLimiter) reserve(host string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)

	if len(l.attempts[host]) >= l.limit {
		return false
	}
	l.attempts[host] = append(l.attempts[host], now)
	return true
}

// release gives back the most recent reservation for host, used when an
// attempt failed for an operational reason rather than a bad password.
// A database outage should not burn through someone's attempts.
func (l *attemptLimiter) release(host string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	times := l.attempts[host]
	if len(times) == 0 {
		return
	}
	if len(times) == 1 {
		delete(l.attempts, host)
		return
	}
	l.attempts[host] = times[:len(times)-1]
}

// remaining reports how many attempts host has left (for tests).
func (l *attemptLimiter) remaining(host string, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	return l.limit - len(l.attempts[host])
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

// clientHost trusts the rightmost X-Forwarded-For hop only when the immediate
// peer belongs to a configured proxy network. Every other request uses the
// socket peer, so clients cannot choose their own rate-limit bucket.
func clientHost(r *http.Request, trustedProxies []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !isTrustedProxy(peer, trustedProxies) {
		return host
	}

	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded == "" {
		return host
	}
	hops := strings.Split(forwarded, ",")
	client, err := netip.ParseAddr(strings.TrimSpace(hops[len(hops)-1]))
	if err != nil {
		return host
	}
	return client.String()
}

func isTrustedProxy(peer netip.Addr, trustedProxies []netip.Prefix) bool {
	for _, prefix := range trustedProxies {
		if prefix.Contains(peer) {
			return true
		}
	}
	return false
}
