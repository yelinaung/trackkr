package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func TestSessionRoundTrip(t *testing.T) {
	t.Parallel()
	codec := newSessionCodec(testSecret, true)
	now := time.Now()

	got, err := codec.decode(codec.encode(42, now.Add(time.Hour)), now)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != 42 {
		t.Errorf("userID = %d, want 42", got)
	}
}

// A correctly signed cookie whose expiry has passed must be rejected
// server-side; the cookie's Expires attribute only binds browsers, and a
// replay with curl never involves one.
func TestSessionRejectsExpiredButValidlySigned(t *testing.T) {
	t.Parallel()
	codec := newSessionCodec(testSecret, true)
	now := time.Now()

	value := codec.encode(42, now.Add(-time.Second))
	if _, err := codec.decode(value, now); !errors.Is(err, errSessionExpired) {
		t.Errorf("err = %v, want errSessionExpired", err)
	}
}

func TestSessionRejectsTamperedValues(t *testing.T) {
	t.Parallel()
	codec := newSessionCodec(testSecret, true)
	now := time.Now()
	valid := codec.encode(42, now.Add(time.Hour))

	tests := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"no signature", "42.99999999999"},
		{"garbage signature", "42.99999999999.notasignature"},
		{"swapped user id", "43." + strings.SplitN(valid, ".", 2)[1]},
		{"signed by another secret", newSessionCodec("ffffffffffffffffffffffffffffffff", true).
			encode(42, now.Add(time.Hour))},
		{"non-numeric user", "abc.99999999999.sig"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := codec.decode(tt.value, now); err == nil {
				t.Errorf("decode(%q) succeeded, want error", tt.value)
			}
		})
	}
}

func TestSessionCookieFlags(t *testing.T) {
	t.Parallel()

	for _, secure := range []bool{true, false} {
		rec := httptest.NewRecorder()
		newSessionCodec(testSecret, secure).setSession(rec, 7)

		cookies := rec.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatalf("cookies = %d, want 1", len(cookies))
		}
		c := cookies[0]

		if !c.HttpOnly {
			t.Error("session cookie is not HttpOnly")
		}
		if c.SameSite != http.SameSiteLaxMode {
			t.Errorf("SameSite = %v, want Lax", c.SameSite)
		}
		if c.Path != "/" {
			t.Errorf("Path = %q, want /", c.Path)
		}
		if c.Secure != secure {
			t.Errorf("Secure = %v, want %v", c.Secure, secure)
		}
	}
}

// The CSRF cookie must not be weaker than the session cookie.
func TestCSRFCookieMatchesSessionFlags(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()

	token, err := newSessionCodec(testSecret, true).issueCSRF(rec)
	if err != nil {
		t.Fatalf("issueCSRF: %v", err)
	}
	if token == "" {
		t.Fatal("issueCSRF returned an empty token")
	}

	c := rec.Result().Cookies()[0]
	if c.Name != csrfCookieName {
		t.Errorf("name = %q, want %q", c.Name, csrfCookieName)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Errorf("csrf cookie flags too weak: %+v", c)
	}
}

func TestClearSessionExpiresCookie(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	newSessionCodec(testSecret, true).clearSession(rec)

	c := rec.Result().Cookies()[0]
	if c.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want negative", c.MaxAge)
	}
	if c.Value != "" {
		t.Errorf("Value = %q, want empty", c.Value)
	}
}

func TestCheckCSRF(t *testing.T) {
	t.Parallel()
	codec := newSessionCodec(testSecret, true)

	newPost := func(cookie, field, header string) *http.Request {
		body := url.Values{}
		if field != "" {
			body.Set(csrfFieldName, field)
		}
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/devices", strings.NewReader(body.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: cookie}) //nolint:gosec // G124: request-side cookie in a test; response flags are asserted separately.
		}
		if header != "" {
			r.Header.Set(csrfHeaderName, header)
		}
		return r
	}

	tests := []struct {
		name                  string
		cookie, field, header string
		want                  bool
	}{
		{name: "matching field", cookie: testCSRFValue, field: testCSRFValue, want: true},
		{name: "matching header", cookie: testCSRFValue, header: testCSRFValue, want: true},
		{name: "missing cookie", field: testCSRFValue},
		{name: "missing field and header", cookie: testCSRFValue},
		{name: "mismatched", cookie: testCSRFValue, field: "other"},
		{name: "both empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := codec.checkCSRF(newPost(tt.cookie, tt.field, tt.header)); got != tt.want {
				t.Errorf("checkCSRF = %v, want %v", got, tt.want)
			}
		})
	}
}
