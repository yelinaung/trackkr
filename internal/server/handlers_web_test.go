package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/db"
	"golang.org/x/crypto/bcrypt"
)

// fakeWeb implements WebQuerier and nothing else, which is the point of
// splitting the interfaces.
type fakeWeb struct {
	users     map[string]*db.UserRow
	byID      map[int64]*db.UserRow
	devices   []db.DeviceRow
	records   []db.ActivityRecordRow
	totals    []db.AppTotalRow
	deleted   []int64
	createErr error
	nextID    int64
}

func newFakeWeb() *fakeWeb {
	return &fakeWeb{
		users:  make(map[string]*db.UserRow),
		byID:   make(map[int64]*db.UserRow),
		nextID: 100,
	}
}

func (f *fakeWeb) addUser(t *testing.T, username, password string) *db.UserRow {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	f.nextID++
	user := &db.UserRow{ID: f.nextID, Username: username, Password: string(hash)}
	f.users[username] = user
	f.byID[user.ID] = user
	return user
}

func (f *fakeWeb) GetUserByID(_ context.Context, id int64) (*db.UserRow, error) {
	u, ok := f.byID[id]
	if !ok {
		return nil, errors.New("no rows")
	}
	return u, nil
}

func (f *fakeWeb) GetUserByUsername(_ context.Context, username string) (*db.UserRow, error) {
	u, ok := f.users[username]
	if !ok {
		return nil, errors.New("no rows")
	}
	return u, nil
}

func (f *fakeWeb) CreateUser(_ context.Context, username, hash string) (*db.UserRow, error) {
	if _, taken := f.users[username]; taken {
		return nil, errors.New("duplicate")
	}
	f.nextID++
	user := &db.UserRow{ID: f.nextID, Username: username, Password: hash}
	f.users[username] = user
	f.byID[user.ID] = user
	return user, nil
}

func (f *fakeWeb) ListDevicesByUser(context.Context, int64) ([]db.DeviceRow, error) {
	return f.devices, nil
}

func (f *fakeWeb) CreateDevice(_ context.Context, userID int64, name, deviceType, apiKey string) (*db.DeviceRow, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.nextID++
	d := db.DeviceRow{
		ID: f.nextID, UserID: userID, Name: name,
		DeviceType: deviceType, APIKey: apiKey, CreatedAt: time.Now(),
	}
	f.devices = append(f.devices, d)
	return &d, nil
}

func (f *fakeWeb) DeleteDevice(_ context.Context, id, _ int64) error {
	f.deleted = append(f.deleted, id)
	kept := f.devices[:0]
	for _, d := range f.devices {
		if d.ID != id {
			kept = append(kept, d)
		}
	}
	f.devices = kept
	return nil
}

func (f *fakeWeb) GetActivityRecords(context.Context, int64, time.Time, time.Time, *int64) ([]db.ActivityRecordRow, error) {
	return f.records, nil
}

func (f *fakeWeb) GetAppTotals(context.Context, int64, time.Time, time.Time, *int64) ([]db.AppTotalRow, error) {
	return f.totals, nil
}

// webServer builds a Server with only the session and web dependencies
// populated; api stays nil because these tests never touch /api/v1.
func webServer(t *testing.T, fake *fakeWeb, allowReg bool) *Server {
	t.Helper()
	logger := zerolog.Nop()

	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}

	srv := &Server{
		config: &Config{
			Server: ServerConfig{Host: testHost, Port: 8080},
			Auth:   AuthConfig{SessionSecret: testSecret, AllowRegistration: allowReg},
		},
		logger:    &logger,
		sessions:  fake,
		web:       fake,
		templates: tmpl,
		codec:     newSessionCodec(testSecret, false),
		limiter:   newAttemptLimiter(loginAttemptLimit, loginAttemptWindow),
		loc:       time.UTC,
	}
	srv.router = newRouter(srv)
	return srv
}

// signIn returns cookies for an authenticated session plus a matching
// CSRF token.
func signIn(t *testing.T, srv *Server, userID int64) (session, csrf *http.Cookie) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.codec.setSession(rec, userID)

	token, err := srv.codec.issueCSRF(rec)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case sessionCookieName:
			session = c
		case csrfCookieName:
			csrf = c
		}
	}
	if session == nil || csrf == nil {
		t.Fatal("failed to build session cookies")
	}
	_ = token
	return session, csrf
}

func formPost(t *testing.T, target string, values url.Values, cookies ...*http.Cookie) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "10.1.2.3:44444"
	for _, c := range cookies {
		r.AddCookie(c)
	}
	return r
}

func TestLoginPageIssuesCSRFCookie(t *testing.T) {
	t.Parallel()
	srv := webServer(t, newFakeWeb(), false)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, testLoginPath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var token string
	for _, c := range rec.Result().Cookies() {
		if c.Name == csrfCookieName {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("no CSRF cookie issued for the pre-session form")
	}
	if !strings.Contains(rec.Body.String(), token) {
		t.Error("CSRF cookie value is not rendered into the form")
	}
}

func TestLoginSucceeds(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "ye", testPassword)
	srv := webServer(t, fake, false)

	_, csrf := signIn(t, srv, user.ID)
	form := url.Values{testUsernameField: {"ye"}, testPasswordField: {testPassword}, csrfFieldName: {csrf.Value}}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, formPost(t, testLoginPath, form, csrf))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}

	var gotSession bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			gotSession = true
		}
	}
	if !gotSession {
		t.Error("no session cookie set after successful login")
	}
}

// Unknown user and wrong password must be indistinguishable.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "ye", testPassword)
	srv := webServer(t, fake, false)
	_, csrf := signIn(t, srv, user.ID)

	bodies := make([]string, 0, 2)
	for _, creds := range []url.Values{
		{testUsernameField: {"ye"}, testPasswordField: {testBadPass}, csrfFieldName: {csrf.Value}},
		{testUsernameField: {"nobody"}, testPasswordField: {testBadPass}, csrfFieldName: {csrf.Value}},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, formPost(t, testLoginPath, creds, csrf))

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}

		// Each render mints a fresh CSRF token, so normalise it out;
		// otherwise the pages differ for a reason that leaks nothing.
		body := rec.Body.String()
		for _, c := range rec.Result().Cookies() {
			if c.Name == csrfCookieName {
				body = strings.ReplaceAll(body, c.Value, "CSRF")
			}
		}
		bodies = append(bodies, body)
	}

	if !strings.Contains(bodies[0], "Invalid username or password") {
		t.Error("error message is not generic")
	}
	if bodies[0] != bodies[1] {
		t.Error("wrong password and unknown user produced different pages, leaking which accounts exist")
	}
}

func TestLoginRequiresCSRF(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	fake.addUser(t, "ye", testPassword)
	srv := webServer(t, fake, false)

	form := url.Values{testUsernameField: {"ye"}, testPasswordField: {testPassword}}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, formPost(t, testLoginPath, form))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 without a CSRF token", rec.Code)
	}
}

func TestLoginThrottlesRepeatedFailures(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "ye", testPassword)
	srv := webServer(t, fake, false)
	_, csrf := signIn(t, srv, user.ID)

	form := url.Values{testUsernameField: {"ye"}, testPasswordField: {testBadPass}, csrfFieldName: {csrf.Value}}
	for i := range loginAttemptLimit {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, formPost(t, testLoginPath, form, csrf))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, formPost(t, testLoginPath, form, csrf))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 after %d failures", rec.Code, loginAttemptLimit)
	}

	// A success clears the bucket, so a legitimate user is not locked
	// out by someone else guessing from the same address.
	good := url.Values{testUsernameField: {"ye"}, testPasswordField: {testPassword}, csrfFieldName: {csrf.Value}}
	srv.limiter.reset("10.1.2.3")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, formPost(t, testLoginPath, good, csrf))
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 after reset", rec.Code)
	}
}

func TestDashboardRequiresSession(t *testing.T) {
	t.Parallel()
	srv := webServer(t, newFakeWeb(), false)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != testLoginPath {
		t.Errorf("Location = %q, want /login", got)
	}
}

// Gating /static would load the login page with no CSS, and the CSP
// leaves no CDN fallback.
func TestStaticAssetsArePublic(t *testing.T) {
	t.Parallel()
	srv := webServer(t, newFakeWeb(), false)

	for _, path := range []string{"/static/style.css", "/static/htmx.min.js", "/static/bootstrap.min.css"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 without a session", path, rec.Code)
		}
	}
}

func TestDashboardRendersTimeline(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "ye", testPassword)
	day := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	fake.devices = []db.DeviceRow{{ID: 7, UserID: user.ID, Name: testLaptop}}
	fake.records = []db.ActivityRecordRow{{
		DeviceID: 7, AppName: testFirefoxLower,
		StartedAt: day.Add(time.Hour), EndedAt: day.Add(2 * time.Hour),
	}}
	fake.totals = []db.AppTotalRow{{AppName: testFirefoxLower, Seconds: 3600}}

	srv := webServer(t, fake, false)
	session, csrf := signIn(t, srv, user.ID)

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?date=2026-05-04", nil)
	r.AddCookie(session)
	r.AddCookie(csrf)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, "<rect") {
		t.Error("no bars rendered")
	}
	if !strings.Contains(body, "1h 00m") {
		t.Error("totals not rendered")
	}
	if !strings.Contains(body, testLaptop) {
		t.Error("device lane label missing")
	}
}

func TestTimelinePartialSwapsWithoutChrome(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "ye", "pw")
	srv := webServer(t, fake, false)
	session, csrf := signIn(t, srv, user.ID)

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/timeline?date=2026-05-04", nil)
	r.Header.Set("HX-Request", "true")
	r.AddCookie(session)
	r.AddCookie(csrf)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<html") {
		t.Error("partial returned a full document; it would nest inside the target div")
	}
}

// An expired session during an htmx swap must redirect the whole page,
// not paint a login form inside the timeline div.
func TestTimelineRedirectsExpiredSessionViaHTMX(t *testing.T) {
	t.Parallel()
	srv := webServer(t, newFakeWeb(), false)

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/timeline", nil)
	r.Header.Set("HX-Request", "true")
	//nolint:gosec // G124: request-side cookie in a test; response flags are asserted separately.
	r.AddCookie(&http.Cookie{
		Name:  sessionCookieName,
		Value: srv.codec.encode(1, time.Now().Add(-time.Hour)),
	})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)

	if got := rec.Header().Get("HX-Redirect"); got != testLoginPath {
		t.Errorf("HX-Redirect = %q, want /login", got)
	}
}

func TestCreateDevice(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "ye", "pw")
	srv := webServer(t, fake, false)
	session, csrf := signIn(t, srv, user.ID)

	form := url.Values{"name": {"work-laptop"}, "device_type": {testLaptop}, csrfFieldName: {csrf.Value}}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, formPost(t, "/devices", form, session, csrf))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if len(fake.devices) != 1 || fake.devices[0].Name != "work-laptop" {
		t.Fatalf("devices = %+v, want one named work-laptop", fake.devices)
	}
	if len(fake.devices[0].APIKey) < 32 {
		t.Errorf("api key %q looks too short to be random", fake.devices[0].APIKey)
	}
}

func TestDeleteDeviceReturnsUpdatedRows(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "ye", "pw")
	fake.devices = []db.DeviceRow{
		{ID: 7, UserID: user.ID, Name: testLaptop, APIKey: "k1"},
		{ID: 8, UserID: user.ID, Name: testDesktop, APIKey: "k2"},
	}
	srv := webServer(t, fake, false)
	session, csrf := signIn(t, srv, user.ID)

	r := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/devices/7", nil)
	r.AddCookie(session)
	r.AddCookie(csrf)
	r.Header.Set(csrfHeaderName, csrf.Value)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != 7 {
		t.Errorf("deleted = %v, want [7]", fake.deleted)
	}

	body := rec.Body.String()
	if strings.Contains(body, "laptop</td>") {
		t.Error("deleted device still listed in the swapped rows")
	}
	if !strings.Contains(body, testDesktop) {
		t.Error("remaining device missing from the swapped rows")
	}
}

func TestDeleteDeviceRequiresCSRFHeader(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "ye", "pw")
	srv := webServer(t, fake, false)
	session, csrf := signIn(t, srv, user.ID)

	r := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/devices/7", nil)
	r.AddCookie(session)
	r.AddCookie(csrf)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 without the CSRF header", rec.Code)
	}
}

func TestRegisterRoutesOnlyWhenAllowed(t *testing.T) {
	t.Parallel()

	closed := webServer(t, newFakeWeb(), false)
	rec := httptest.NewRecorder()
	closed.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/register", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when registration is disabled", rec.Code)
	}

	open := webServer(t, newFakeWeb(), true)
	rec = httptest.NewRecorder()
	open.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/register", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 when registration is enabled", rec.Code)
	}
}

func TestRegisterCreatesUserAndSession(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	srv := webServer(t, fake, true)
	_, csrf := signIn(t, srv, 1)

	form := url.Values{
		"username":    {"newcomer"},
		"password":    {"a-long-enough-password"},
		csrfFieldName: {csrf.Value},
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, formPost(t, "/register", form, csrf))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if _, ok := fake.users["newcomer"]; !ok {
		t.Error("user was not created")
	}
}

func TestRegisterRejectsShortPassword(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	srv := webServer(t, fake, true)
	_, csrf := signIn(t, srv, 1)

	form := url.Values{testUsernameField: {"newcomer"}, testPasswordField: {"short"}, csrfFieldName: {csrf.Value}}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, formPost(t, "/register", form, csrf))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if len(fake.users) != 0 {
		t.Error("a user was created despite the short password")
	}
}

func TestLogoutClearsSession(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "ye", "pw")
	srv := webServer(t, fake, false)
	session, csrf := signIn(t, srv, user.ID)

	form := url.Values{csrfFieldName: {csrf.Value}}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, formPost(t, "/logout", form, session, csrf))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge >= 0 {
			t.Errorf("session cookie not cleared: %+v", c)
		}
	}
}

// A day holding exactly the limit fits: the query fetches one extra row
// as a probe, so only its presence means anything was cut.
func TestDashboardTruncationBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		records   int
		wantNotic bool
	}{
		{"one under the limit", db.ActivityRecordLimit - 1, false},
		{"exactly the limit", db.ActivityRecordLimit, false},
		{"one over the limit", db.ActivityRecordLimit + 1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeWeb()
			user := fake.addUser(t, "boundary-"+tt.name, testPassword)
			day := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)

			fake.records = make([]db.ActivityRecordRow, 0, tt.records)
			for i := range tt.records {
				fake.records = append(fake.records, db.ActivityRecordRow{
					DeviceID:  7,
					AppName:   fmt.Sprintf("app-%d", i%5),
					StartedAt: day.Add(time.Duration(i) * time.Second),
					EndedAt:   day.Add(time.Duration(i)*time.Second + time.Second),
				})
			}
			fake.devices = []db.DeviceRow{{ID: 7, UserID: user.ID, Name: testLaptop}}

			srv := webServer(t, fake, false)
			session, csrf := signIn(t, srv, user.ID)

			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?date=2026-05-04", nil)
			r.AddCookie(session)
			r.AddCookie(csrf)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, r)

			body := rec.Body.String()
			gotNotice := strings.Contains(body, "totals below cover the whole day")
			if gotNotice != tt.wantNotic {
				t.Errorf("truncation notice = %v, want %v", gotNotice, tt.wantNotic)
			}

			// The probe row must never be drawn.
			if bars := strings.Count(body, "<rect"); bars > db.ActivityRecordLimit {
				t.Errorf("rendered %d bars, want at most %d", bars, db.ActivityRecordLimit)
			}
		})
	}
}
