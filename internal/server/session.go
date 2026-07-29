package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookieName = "trackkr_session"
	csrfCookieName    = "trackkr_csrf"
	csrfFieldName     = "csrf_token"
	csrfHeaderName    = "X-CSRF-Token"

	sessionTTL = 7 * 24 * time.Hour

	// minSecretLen is the shortest session secret that is not trivially
	// brute-forced; below this, cookies are forgeable.
	minSecretLen = 32
)

var (
	errBadSession     = errors.New("invalid session cookie")
	errSessionExpired = errors.New("session expired")
)

// sessionCodec signs and verifies session cookies. There is no session
// store: the cookie carries the user ID and its own expiry, and the HMAC
// makes both unforgeable.
type sessionCodec struct {
	secret []byte
	secure bool
}

func newSessionCodec(secret string, secure bool) *sessionCodec {
	return &sessionCodec{secret: []byte(secret), secure: secure}
}

// encode returns the cookie value for a user session expiring at exp.
func (c *sessionCodec) encode(userID int64, exp time.Time) string {
	payload := fmt.Sprintf("%d.%d", userID, exp.Unix())
	return payload + "." + c.sign(payload)
}

// decode verifies the signature and then the expiry. The cookie's own
// Expires attribute is a browser courtesy; an attacker replaying a
// captured value never involves a browser, so freshness is checked here.
func (c *sessionCodec) decode(value string, now time.Time) (int64, error) {
	idx := strings.LastIndex(value, ".")
	if idx < 0 {
		return 0, errBadSession
	}
	payload, sig := value[:idx], value[idx+1:]

	if !hmac.Equal([]byte(sig), []byte(c.sign(payload))) {
		return 0, errBadSession
	}

	userID, expUnix, err := splitPayload(payload)
	if err != nil {
		return 0, err
	}
	if !now.Before(time.Unix(expUnix, 0)) {
		return 0, errSessionExpired
	}
	return userID, nil
}

func splitPayload(payload string) (userID, expUnix int64, err error) {
	parts := strings.Split(payload, ".")
	if len(parts) != 2 {
		return 0, 0, errBadSession
	}
	userID, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, errBadSession
	}
	expUnix, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, errBadSession
	}
	return userID, expUnix, nil
}

func (c *sessionCodec) sign(payload string) string {
	mac := hmac.New(sha256.New, c.secret)
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (c *sessionCodec) siteIconSignature(userID int64, site string) string {
	return c.sign(fmt.Sprintf("site-icon.%d.%s", userID, site))
}

func (c *sessionCodec) validSiteIconSignature(userID int64, site, signature string) bool {
	if signature == "" {
		return false
	}
	return hmac.Equal(
		[]byte(signature),
		[]byte(c.siteIconSignature(userID, site)),
	)
}

// setSession issues the session cookie.
func (c *sessionCodec) setSession(w http.ResponseWriter, userID int64) {
	exp := time.Now().Add(sessionTTL)
	http.SetCookie(w, c.cookie(sessionCookieName, c.encode(userID, exp), exp))
}

// clearSession expires the session cookie.
func (c *sessionCodec) clearSession(w http.ResponseWriter) {
	//nolint:gosec // G124: cookie() sets the flags; Secure follows server.secure_cookies.
	cookie := c.cookie(sessionCookieName, "", time.Unix(0, 0))
	cookie.MaxAge = -1
	http.SetCookie(w, cookie)
}

// issueCSRF sets a fresh CSRF cookie and returns its token so the caller
// can render it into the form. The cookie is HttpOnly because the server
// renders the field; nothing needs to read it from script.
func (c *sessionCodec) issueCSRF(w http.ResponseWriter) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating csrf token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	http.SetCookie(w, c.cookie(csrfCookieName, token, time.Now().Add(sessionTTL)))
	return token, nil
}

// checkCSRF compares the cookie against the submitted field or header.
func (c *sessionCodec) checkCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}

	sent := r.Header.Get(csrfHeaderName)
	if sent == "" {
		sent = r.PostFormValue(csrfFieldName)
	}
	if sent == "" {
		return false
	}
	return hmac.Equal([]byte(cookie.Value), []byte(sent))
}

// cookie builds a cookie with the flags shared by session and CSRF: the
// CSRF token travelling in clear while the session is protected would
// defeat the point, so both follow secure_cookies.
func (c *sessionCodec) cookie(name, value string, exp time.Time) *http.Cookie {
	//nolint:gosec // G124: Secure is config-driven (server.secure_cookies), not a literal.
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		Secure:   c.secure,
		SameSite: http.SameSiteLaxMode,
	}
}
