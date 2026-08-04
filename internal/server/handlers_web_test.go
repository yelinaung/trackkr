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
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/db"
	"golang.org/x/crypto/bcrypt"
)

const testRecordActivityURL = "/activity?kind=app&name=code"

// fakeWeb implements WebQuerier and nothing else, which is the point of
// splitting the interfaces.
type fakeWeb struct {
	users    map[string]*db.UserRow
	byID     map[int64]*db.UserRow
	devices  []db.DeviceRow
	records  []db.ActivityRecordRow
	totals   []db.AppTotalRow
	sites    []db.SiteTotalRow
	deleted  []int64
	nextID   int64
	icons    []db.AppIconRow
	iconErr  error
	iconKeys []string

	categories        []db.CategorySummaryRow
	knownApplications []db.KnownApplicationRow
	assignments       map[string]db.CategoryRow
	editablePage      db.EditableActivityPage
	overrideRecordID  int64
	overrideCategory  *int64

	activityStart time.Time
	activityEnd   time.Time
	siteStart     time.Time
	siteEnd       time.Time

	// lookupErr and createErr stand in for operational failures, as
	// opposed to the pgx.ErrNoRows a missing row produces.
	lookupErr error
	createErr error
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
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	u, ok := f.byID[id]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return u, nil
}

func (f *fakeWeb) GetUserByUsername(_ context.Context, username string) (*db.UserRow, error) {
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	u, ok := f.users[username]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return u, nil
}

func (f *fakeWeb) CreateUser(_ context.Context, username, hash string) (*db.UserRow, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	if _, taken := f.users[username]; taken {
		// What PostgreSQL actually returns for a duplicate key.
		return nil, &pgconn.PgError{Code: uniqueViolation}
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

func (f *fakeWeb) GetActivitySummary(
	_ context.Context,
	_ int64,
	start, end time.Time,
	_ *int64,
) (*db.ActivitySummary, error) {
	f.activityStart = start
	f.activityEnd = end
	records := f.records
	truncated := len(records) > db.ActivityRecordLimit
	if truncated {
		records = records[:db.ActivityRecordLimit]
	}
	return &db.ActivitySummary{
		Records:           records,
		Totals:            f.totals,
		TimelineTruncated: truncated,
	}, nil
}

func (f *fakeWeb) GetSiteTotals(
	_ context.Context,
	_ int64,
	start, end time.Time,
	_ *int64,
) ([]db.SiteTotalRow, error) {
	f.siteStart = start
	f.siteEnd = end
	return f.sites, nil
}

func (f *fakeWeb) ListCategories(context.Context, int64) ([]db.CategorySummaryRow, error) {
	return f.categories, nil
}

func (f *fakeWeb) CreateCategory(_ context.Context, userID int64, name, colorKey string) (*db.CategoryRow, error) {
	f.nextID++
	category := db.CategoryRow{ID: f.nextID, UserID: userID, Name: name, ColorKey: colorKey}
	f.categories = append(f.categories, db.CategorySummaryRow{CategoryRow: category})
	return &category, nil
}

func (f *fakeWeb) UpdateCategory(_ context.Context, userID, categoryID int64, name, colorKey string) (*db.CategoryRow, error) {
	for i := range f.categories {
		category := &f.categories[i]
		if category.ID == categoryID && category.UserID == userID {
			category.Name = name
			category.ColorKey = colorKey
			return &category.CategoryRow, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeWeb) DeleteCategory(_ context.Context, userID, categoryID int64) error {
	for i := range f.categories {
		if f.categories[i].ID == categoryID && f.categories[i].UserID == userID {
			f.categories = append(f.categories[:i], f.categories[i+1:]...)
			return nil
		}
	}
	return pgx.ErrNoRows
}

func (f *fakeWeb) ListKnownApplications(context.Context, int64, time.Time, int) ([]db.KnownApplicationRow, error) {
	return f.knownApplications, nil
}

func (f *fakeWeb) ListAppCategoryAssignments(context.Context, int64, []string) (map[string]db.CategoryRow, error) {
	return f.assignments, nil
}

func (f *fakeWeb) SetAppCategory(_ context.Context, _ int64, appKey string, categoryID *int64) error {
	if f.assignments == nil {
		f.assignments = make(map[string]db.CategoryRow)
	}
	if categoryID == nil {
		delete(f.assignments, appKey)
		return nil
	}
	for _, category := range f.categories {
		if category.ID == *categoryID {
			f.assignments[appKey] = category.CategoryRow
			return nil
		}
	}
	return pgx.ErrNoRows
}

func (f *fakeWeb) ListEditableActivityRecords(context.Context, int64, *db.EditableActivityFilter) (*db.EditableActivityPage, error) {
	return &f.editablePage, nil
}

func (f *fakeWeb) SetActivityRecordCategoryOverride(_ context.Context, _ int64, recordID int64, categoryID *int64) error {
	f.overrideRecordID = recordID
	f.overrideCategory = categoryID
	return nil
}

func (f *fakeWeb) DeleteActivityRecordCategoryOverride(context.Context, int64, int64) error {
	return nil
}

func (f *fakeWeb) SetActivityRecordApplicationCategory(context.Context, int64, int64, *int64) error {
	return nil
}

func (f *fakeWeb) AppIconMetadata(_ context.Context, userID int64, keys []string) ([]db.AppIconRow, error) {
	if f.iconErr != nil {
		return nil, f.iconErr
	}
	f.iconKeys = append([]string(nil), keys...)
	wanted := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		wanted[key] = struct{}{}
	}
	var rows []db.AppIconRow
	for i := range f.icons {
		row := &f.icons[i]
		if row.UserID != userID {
			continue
		}
		if _, ok := wanted[row.AppKey]; ok {
			rows = append(rows, *row)
		}
	}
	return rows, nil
}

func (f *fakeWeb) AppIcon(_ context.Context, userID, id int64) (*db.AppIconRow, error) {
	if f.iconErr != nil {
		return nil, f.iconErr
	}
	for i := range f.icons {
		if f.icons[i].UserID == userID && f.icons[i].ID == id {
			return &f.icons[i], nil
		}
	}
	return nil, pgx.ErrNoRows
}

func TestCategoriesPageAndApplicationDefault(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "categories", testPassword)
	category := db.CategoryRow{ID: 7, UserID: user.ID, Name: "Work", ColorKey: "sky"}
	fake.categories = []db.CategorySummaryRow{{CategoryRow: category, AssignedAppCount: 1}}
	fake.knownApplications = []db.KnownApplicationRow{{
		AppKey: testAppCode, AppName: testAppCode, LastSeen: time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC),
	}}
	fake.assignments = map[string]db.CategoryRow{testAppCode: category}
	srv := webServer(t, fake, false)
	session, csrf := signIn(t, srv, user.ID)

	page := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/categories", nil)
	page.AddCookie(session)
	page.AddCookie(csrf)
	pageRec := httptest.NewRecorder()
	srv.ServeHTTP(pageRec, page)
	if pageRec.Code != http.StatusOK {
		t.Fatalf("GET /categories = %d, want 200: %s", pageRec.Code, pageRec.Body.String())
	}
	for _, want := range []string{"New category", "Work", testAppCode, db.UncategorizedCategoryName} {
		if !strings.Contains(pageRec.Body.String(), want) {
			t.Errorf("category page missing %q", want)
		}
	}

	form := url.Values{"app_key": {testAppCode}, "category_id": {""}, csrfFieldName: {csrf.Value}}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/categories/assignments", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(session)
	request.AddCookie(csrf)
	response := httptest.NewRecorder()
	srv.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("POST /categories/assignments = %d, want 303: %s", response.Code, response.Body.String())
	}
	if _, ok := fake.assignments[testAppCode]; ok {
		t.Error("clearing an application category did not remove the assignment")
	}
}

func TestRecordCategoryRejectsInvalidScopeAction(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "record-category", testPassword)
	srv := webServer(t, fake, false)
	session, csrf := signIn(t, srv, user.ID)

	form := url.Values{
		categoryScopeField:  {categoryScopeApplication},
		categoryActionField: {categoryActionInherit},
		csrfFieldName:       {csrf.Value},
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/activity/records/3/category", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(session)
	request.AddCookie(csrf)
	response := httptest.NewRecorder()
	srv.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Errorf("invalid application inherit action = %d, want 400", response.Code)
	}
}

func TestRecordCategoryStoresExplicitUncategorized(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "record-uncategorized", testPassword)
	srv := webServer(t, fake, false)
	session, csrf := signIn(t, srv, user.ID)

	form := url.Values{
		categoryScopeField:  {categoryScopeRecord},
		categoryActionField: {categoryActionNone},
		csrfFieldName:       {csrf.Value},
		"return_to":         {testRecordActivityURL},
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/activity/records/3/category", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(session)
	request.AddCookie(csrf)
	response := httptest.NewRecorder()
	srv.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("record Uncategorized = %d, want 303: %s", response.Code, response.Body.String())
	}
	if fake.overrideRecordID != 3 || fake.overrideCategory != nil {
		t.Errorf("override call = record %d category %v, want record 3 explicit nil", fake.overrideRecordID, fake.overrideCategory)
	}
	if got := response.Header().Get("Location"); got != testRecordActivityURL {
		t.Errorf("redirect = %q, want original detail URL", got)
	}
}

func TestRecordCategoryHTMXRedirectsTheWholePage(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "record-htmx", testPassword)
	srv := webServer(t, fake, false)
	session, csrf := signIn(t, srv, user.ID)

	form := url.Values{
		categoryScopeField:  {categoryScopeRecord},
		categoryActionField: {categoryActionNone},
		csrfFieldName:       {csrf.Value},
		"return_to":         {"/activity?name=code&kind=app"},
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/activity/records/3/category", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set(htmxRequestHeader, htmxRequestValue)
	request.AddCookie(session)
	request.AddCookie(csrf)
	response := httptest.NewRecorder()
	srv.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("HTMX record category = %d, want 204: %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("HX-Redirect"); got != testRecordActivityURL {
		t.Errorf("HX-Redirect = %q, want re-encoded detail URL", got)
	}
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
		iconRead:  fake,
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

func TestDashboardWeeklyViewUsesCalendarWeek(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "weekly", testPassword)
	weekStart := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	weekEnd := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	fake.devices = []db.DeviceRow{{ID: 7, UserID: user.ID, Name: testLaptop}}
	fake.records = []db.ActivityRecordRow{{
		DeviceID: 7, AppName: testFirefoxLower,
		StartedAt: weekStart.Add(6*24*time.Hour + time.Hour),
		EndedAt:   weekStart.Add(6*24*time.Hour + 2*time.Hour),
	}}

	srv := webServer(t, fake, false)
	session, csrf := signIn(t, srv, user.ID)

	r := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"/?date=2026-05-06&view=week",
		nil,
	)
	r.AddCookie(session)
	r.AddCookie(csrf)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !fake.activityStart.Equal(weekStart) || !fake.activityEnd.Equal(weekEnd) {
		t.Errorf("activity range = %v..%v, want %v..%v", fake.activityStart, fake.activityEnd, weekStart, weekEnd)
	}
	if !fake.siteStart.Equal(weekStart) || !fake.siteEnd.Equal(weekEnd) {
		t.Errorf("site range = %v..%v, want %v..%v", fake.siteStart, fake.siteEnd, weekStart, weekEnd)
	}

	body := rec.Body.String()
	for _, want := range []string{
		`value="week" checked`,
		`value="2026-05-06"`,
		"4-10 May 2026",
		"Mon 4",
		"Sun 10",
		"Sun 10 01:00 - Sun 10 02:00",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("weekly dashboard is missing %q", want)
		}
	}
}

func TestDashboardTotalsUseTwoTenItemBatches(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "summary-pages", testPassword)
	day := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	fake.devices = []db.DeviceRow{{ID: 7, UserID: user.ID, Name: testLaptop}}
	fake.records = []db.ActivityRecordRow{{
		DeviceID: 7, AppName: testFirefoxLower,
		StartedAt: day.Add(time.Hour), EndedAt: day.Add(2 * time.Hour),
	}}
	for index := 1; index <= dashboardTotalLimit+1; index++ {
		fake.totals = append(fake.totals, db.AppTotalRow{
			AppName: fmt.Sprintf("App %02d", index),
			Seconds: int64(dashboardTotalLimit + 2 - index),
		})
		fake.sites = append(fake.sites, db.SiteTotalRow{
			Site:    fmt.Sprintf("site-%02d.example.com", index),
			Seconds: int64(dashboardTotalLimit + 2 - index),
		})
	}

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
	if len(fake.iconKeys) != dashboardTotalLimit {
		t.Errorf("icon metadata keys = %d, want %d", len(fake.iconKeys), dashboardTotalLimit)
	}

	body := rec.Body.String()
	if got := strings.Count(body, `<details class="totals__more">`); got != 2 {
		t.Errorf("load-more controls = %d, want 2", got)
	}
	if got := strings.Count(body, "Load 10 more"); got != 2 {
		t.Errorf("ten-item load-more labels = %d, want 2", got)
	}
	for _, want := range []string{"App 10", "App 11", "App 20", "site-10.example.com", "site-11.example.com", "site-20.example.com"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard is missing %q from its two batches", want)
		}
	}
	for _, excluded := range []string{"App 21", "site-21.example.com"} {
		if strings.Contains(body, excluded) {
			t.Errorf("dashboard rendered %q beyond its two batches", excluded)
		}
	}
}

func TestDashboardSignsOnlyFetchableSiteIcons(t *testing.T) {
	t.Parallel()

	fake := newFakeWeb()
	user := fake.addUser(t, "site-dashboard", testPassword)
	fake.records = []db.ActivityRecordRow{{
		DeviceID: 1, AppName: testFirefoxLower,
		StartedAt: time.Date(2026, 5, 4, 1, 0, 0, 0, time.UTC),
		EndedAt:   time.Date(2026, 5, 4, 2, 0, 0, 0, time.UTC),
	}}
	fake.sites = []db.SiteTotalRow{
		{Site: testSiteHost, Seconds: 300},
		{Site: testHost, Seconds: 60},
	}

	srv := webServer(t, fake, false)
	session, csrf := signIn(t, srv, user.ID)
	request := newRequest(t, http.MethodGet, "/?date=2026-05-04", nil)
	request.AddCookie(session)
	request.AddCookie(csrf)
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `src="/site-icons/example.com?sig=`) {
		t.Error("canonical public site lacks a signed icon URL")
	}
	if strings.Contains(body, `/site-icons/127.0.0.1`) {
		t.Error("IP site received a fetchable icon URL")
	}
	if !strings.Contains(body, `class="totals__icon totals__monogram"`) {
		t.Error("non-fetchable site lacks a monogram fallback")
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
		"username":    {testNewUser},
		"password":    {testGoodPassword},
		csrfFieldName: {csrf.Value},
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, formPost(t, "/register", form, csrf))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if _, ok := fake.users[testNewUser]; !ok {
		t.Error("user was not created")
	}
}

func TestRegisterRejectsShortPassword(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	srv := webServer(t, fake, true)
	_, csrf := signIn(t, srv, 1)

	form := url.Values{testUsernameField: {testNewUser}, testPasswordField: {"short"}, csrfFieldName: {csrf.Value}}
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
			gotNotice := strings.Contains(body, "totals below cover the selected range")
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

// A database outage is not a credential failure: the visitor should see
// a server error, and the attempt they reserved must be handed back.
func TestLoginSurfacesOperationalErrors(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "ye", testPassword)
	srv := webServer(t, fake, false)
	_, csrf := signIn(t, srv, user.ID)

	fake.lookupErr = errors.New("connection refused")

	form := url.Values{testUsernameField: {"ye"}, testPasswordField: {testPassword}, csrfFieldName: {csrf.Value}}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, formPost(t, testLoginPath, form, csrf))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Invalid username or password") {
		t.Error("an outage was reported as bad credentials")
	}
	if left := srv.limiter.remaining("10.1.2.3", time.Now()); left != loginAttemptLimit {
		t.Errorf("remaining attempts = %d, want %d; the outage consumed one", left, loginAttemptLimit)
	}
}

func TestRegisterDistinguishesTakenFromBroken(t *testing.T) {
	t.Parallel()

	t.Run("duplicate username", func(t *testing.T) {
		t.Parallel()
		fake := newFakeWeb()
		fake.addUser(t, "taken", testPassword)
		srv := webServer(t, fake, true)
		_, csrf := signIn(t, srv, 1)

		form := url.Values{
			testUsernameField: {"taken"},
			testPasswordField: {testGoodPassword},
			csrfFieldName:     {csrf.Value},
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, formPost(t, "/register", form, csrf))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "username is taken") {
			t.Error("duplicate was not reported as taken")
		}
	})

	t.Run("database failure", func(t *testing.T) {
		t.Parallel()
		fake := newFakeWeb()
		fake.createErr = errors.New("connection refused")
		srv := webServer(t, fake, true)
		_, csrf := signIn(t, srv, 1)

		form := url.Values{
			testUsernameField: {testNewUser},
			testPasswordField: {testGoodPassword},
			csrfFieldName:     {csrf.Value},
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, formPost(t, "/register", form, csrf))

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "username is taken") {
			t.Error("an outage told the visitor to pick another name")
		}
	})
}

// bcrypt rejects anything over 72 bytes, and a non-ASCII passphrase
// reaches that well before 72 characters.
func TestRegisterRejectsOverlongPassword(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	srv := webServer(t, fake, true)
	_, csrf := signIn(t, srv, 1)

	// 30 characters, 90 bytes.
	long := strings.Repeat("パスワード", 6)
	if len(long) <= maxPasswordBytes {
		t.Fatalf("test fixture is only %d bytes", len(long))
	}

	form := url.Values{
		testUsernameField: {testNewUser},
		testPasswordField: {long},
		csrfFieldName:     {csrf.Value},
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, formPost(t, "/register", form, csrf))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (not a 500 from bcrypt)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "too long") {
		t.Errorf("no explanation of the byte limit:\n%s", rec.Body.String())
	}
	if len(fake.users) != 0 {
		t.Error("a user was created despite the overlong password")
	}
}

// Registration hashes with bcrypt, so it needs the same throttle as
// login or it is simply the cheaper target.
func TestRegisterIsThrottled(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	srv := webServer(t, fake, true)
	_, csrf := signIn(t, srv, 1)

	form := url.Values{testUsernameField: {""}, testPasswordField: {"short"}, csrfFieldName: {csrf.Value}}
	for range loginAttemptLimit {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, formPost(t, "/register", form, csrf))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, formPost(t, "/register", form, csrf))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
}

// A signed-in visitor who never loaded /login still needs a usable token
// on the pages that render forms.
func TestDevicesPageMintsCSRFTokenWhenMissing(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "ye", testPassword)
	srv := webServer(t, fake, false)
	session, _ := signIn(t, srv, user.ID)

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/devices", nil)
	r.AddCookie(session)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)

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
		t.Fatal("no CSRF cookie minted for a page with forms")
	}
	if !strings.Contains(rec.Body.String(), token) {
		t.Error("minted token was not rendered into the form")
	}
}

// http.FileServer happily indexes an embedded FS otherwise.
func TestStaticDirectoryListingIsBlocked(t *testing.T) {
	t.Parallel()
	srv := webServer(t, newFakeWeb(), false)

	for _, path := range []string{"/static/", "/static/fonts/"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "style.css") {
			t.Errorf("%s leaked a directory listing", path)
		}
	}
}

// len() counts bytes, so a byte-based minimum lets four three-byte CJK
// characters pass as "12" -- shorter than the form's minlength and than
// the message the server itself returns.
func TestRegisterCountsPasswordCharactersNotBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		password string
		wantCode int
	}{
		{"four CJK characters, twelve bytes", "密码密码", http.StatusBadRequest},
		{"eleven ascii characters", "elevenchars", http.StatusBadRequest},
		{"twelve ascii characters", "twelvechars!", http.StatusSeeOther},
		{"twelve CJK characters", "密码密码密码密码密码密码", http.StatusSeeOther},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeWeb()
			srv := webServer(t, fake, true)
			_, csrf := signIn(t, srv, 1)

			form := url.Values{
				testUsernameField: {"user-" + tt.name},
				testPasswordField: {tt.password},
				csrfFieldName:     {csrf.Value},
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, formPost(t, "/register", form, csrf))

			if rec.Code != tt.wantCode {
				t.Errorf("password %q (%d chars, %d bytes): status = %d, want %d",
					tt.password, utf8.RuneCountInString(tt.password), len(tt.password),
					rec.Code, tt.wantCode)
			}
		})
	}
}

// Every unique username succeeds, so returning the attempt on success
// would leave account creation completely unthrottled: one host could
// mint accounts forever, each costing a bcrypt hash and a row.
func TestRegisterSuccessKeepsThrottleReservation(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	srv := webServer(t, fake, true)
	_, csrf := signIn(t, srv, 1)

	register := func(username string) int {
		form := url.Values{
			testUsernameField: {username},
			testPasswordField: {testGoodPassword},
			csrfFieldName:     {csrf.Value},
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, formPost(t, "/register", form, csrf))
		return rec.Code
	}

	for i := range loginAttemptLimit {
		if code := register(fmt.Sprintf("newcomer-%d", i)); code != http.StatusSeeOther {
			t.Fatalf("registration %d: status = %d, want 303", i+1, code)
		}
	}

	if code := register("one-too-many"); code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 once the window is spent", code)
	}
	if len(fake.users) != loginAttemptLimit {
		t.Errorf("created %d accounts, want %d", len(fake.users), loginAttemptLimit)
	}
}

// Six emoji are 12 UTF-16 code units but 6 characters. Whatever the
// answer, the browser and the server must give the same one.
func TestRegisterRejectsSixEmojiConsistently(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	srv := webServer(t, fake, true)
	_, csrf := signIn(t, srv, 1)

	emoji := "🙂🙂🙂🙂🙂🙂"
	if utf8.RuneCountInString(emoji) != 6 {
		t.Fatalf("fixture is %d characters, want 6", utf8.RuneCountInString(emoji))
	}

	form := url.Values{
		testUsernameField: {testNewUser},
		testPasswordField: {emoji},
		csrfFieldName:     {csrf.Value},
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, formPost(t, "/register", form, csrf))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: six characters is under the minimum", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "at least 12 characters") {
		t.Error("rejection does not restate the policy")
	}
}

// Switching the period swaps the chart but not the address bar, so without a
// pushed URL a reload re-renders the period the page was opened with while the
// browser restores the controls the user last set: "Day" selected above a week
// of bars.
func TestFilterPartialsPushTheSelectedPeriod(t *testing.T) {
	t.Parallel()

	day := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	device := int64(7)

	cases := []struct {
		name  string
		path  string
		query url.Values
		win   *dashboardWindow
		want  string
	}{
		{
			name: "day",
			path: "/",
			win:  &dashboardWindow{day: day, view: dashboardViewDay},
			want: "/?date=2026-08-03&view=day",
		},
		{
			name: "week with a device filter",
			path: "/",
			win:  &dashboardWindow{day: day, view: dashboardViewWeek, deviceID: &device},
			want: "/?date=2026-08-03&device=7&view=week",
		},
		{
			name:  "detail keeps its subject",
			path:  "/activity",
			query: url.Values{"kind": {"app"}, "name": {"Google Chrome"}},
			win:   &dashboardWindow{day: day, view: dashboardViewDay},
			want:  "/activity?date=2026-08-03&kind=app&name=Google+Chrome&view=day",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			pushFilterURL(rec, tc.path, tc.query, tc.win)

			if got := rec.Header().Get("HX-Push-Url"); got != tc.want {
				t.Errorf("HX-Push-Url = %q, want %q", got, tc.want)
			}
		})
	}
}

// A rolling view is anchored to the clock, not to the date field, so it must
// ignore whatever date the URL is carrying rather than reading it back as an
// anchor.
func TestRollingWindowIgnoresTheDateField(t *testing.T) {
	t.Parallel()

	h := &webHandlers{loc: time.UTC}
	before := time.Now().In(time.UTC)
	win := h.parseWindow(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?date=2020-01-01&view=6h", nil))
	after := time.Now().In(time.UTC)

	if win.view != dashboardViewHour6 {
		t.Fatalf("view = %q, want %q", win.view, dashboardViewHour6)
	}
	if got := win.end.Sub(win.start); got != 6*time.Hour {
		t.Errorf("span = %s, want 6h", got)
	}
	if win.end.Before(before.Truncate(time.Minute)) || win.end.After(after) {
		t.Errorf("window ends at %s, want the present", win.end)
	}
	if win.day.Year() == 2020 {
		t.Error("window took its day from the stale date parameter")
	}
}

// The date is left out of a rolling URL entirely: a reload of a link written
// an hour ago should still mean "the last six hours", not six hours ending
// whenever it was copied.
func TestRollingFilterURLCarriesNoDate(t *testing.T) {
	t.Parallel()

	h := &webHandlers{loc: time.UTC}
	win := h.parseWindow(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?view=12h", nil))

	rec := httptest.NewRecorder()
	pushFilterURL(rec, "/", nil, win)

	if got := rec.Header().Get("HX-Push-Url"); got != "/?view=12h" {
		t.Errorf("HX-Push-Url = %q, want %q", got, "/?view=12h")
	}
}

func TestRollingWindowLabelsTheSpanAndClock(t *testing.T) {
	t.Parallel()

	end := time.Date(2026, 8, 3, 20, 20, 0, 0, time.UTC)
	win := &dashboardWindow{view: dashboardViewHour6, start: end.Add(-6 * time.Hour), end: end}

	if got, want := win.label(), "Last 6 hours, 14:20 - 20:20"; got != want {
		t.Errorf("label() = %q, want %q", got, want)
	}
}
