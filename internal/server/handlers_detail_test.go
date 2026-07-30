package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/db"
)

const testDetailDay = "2026-05-04"

// detailFixture is a day with two applications on one device: an hour of the
// subject, an hour of something else, then half an hour of the subject again.
func detailFixture(t *testing.T) (*fakeWeb, *Server, *db.UserRow) {
	t.Helper()
	fake := newFakeWeb()
	user := fake.addUser(t, "detail", testPassword)
	day := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)

	fake.devices = []db.DeviceRow{{ID: 7, UserID: user.ID, Name: testLaptop}}
	fake.records = []db.ActivityRecordRow{
		{
			DeviceID: 7, AppName: testAppCode, Title: testWindowTitle,
			StartedAt: day.Add(9 * time.Hour), EndedAt: day.Add(10 * time.Hour),
		},
		{
			DeviceID: 7, AppName: testFirefoxLower,
			StartedAt: day.Add(10 * time.Hour), EndedAt: day.Add(11 * time.Hour),
		},
		{
			DeviceID: 7, AppName: testAppCode, Title: "style.css",
			StartedAt: day.Add(11 * time.Hour), EndedAt: day.Add(11*time.Hour + 30*time.Minute),
		},
	}
	fake.totals = []db.AppTotalRow{
		{AppName: testAppCode, Seconds: 5400},
		{AppName: db.FirefoxAppName, Seconds: 3600},
	}

	return fake, webServer(t, fake, false), user
}

// getDetail signs in and fetches one detail URL.
func getDetail(t *testing.T, srv *Server, userID int64, target string) *httptest.ResponseRecorder {
	t.Helper()
	session, csrf := signIn(t, srv, userID)

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	r.AddCookie(session)
	r.AddCookie(csrf)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	return rec
}

func TestActivityDetailShowsOneApplication(t *testing.T) {
	t.Parallel()
	_, srv, user := detailFixture(t)

	rec := getDetail(t, srv, user.ID, "/activity?kind=app&name=code&date="+testDetailDay)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	for _, want := range []string{
		"Application",
		// The headline is the same number the dashboard row carried.
		"1h 30m",
		// 5400 of 9000 seconds tracked.
		"60%",
		// Two sessions, the longest an hour.
		`class="session"`,
		"09:00",
		"11:30",
		"1h 00m",
		// The window titles the subject was used in.
		testWindowTitle,
		"style.css",
		// And the way back, with the day still selected.
		"/?date=2026-05-04&amp;view=day",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page is missing %q", want)
		}
	}
}

// The point of the detail chart: the subject stands out against the rest of
// the day rather than being shown in isolation.
func TestActivityDetailMutesTheRestOfTheDay(t *testing.T) {
	t.Parallel()
	_, srv, user := detailFixture(t)

	body := getDetail(t, srv, user.ID, "/activity?kind=app&name=code&date="+testDetailDay).Body.String()
	if !strings.Contains(body, "bar--muted") {
		t.Error("no muted context bars: the other application is missing from the chart")
	}
	if strings.Count(body, "<rect") < 4 {
		t.Errorf("want context and focus bars, got %d rects", strings.Count(body, "<rect"))
	}
}

// A browser stored under a platform alias has to reach its own detail page:
// the totals row says "Google Chrome" and the records say "google-chrome".
func TestActivityDetailFoldsBrowserAliases(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "aliases", testPassword)
	day := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)

	fake.devices = []db.DeviceRow{{ID: 7, UserID: user.ID, Name: testLaptop}}
	fake.records = []db.ActivityRecordRow{{
		DeviceID: 7, AppName: "google-chrome",
		StartedAt: day.Add(9 * time.Hour), EndedAt: day.Add(10 * time.Hour),
	}}
	fake.totals = []db.AppTotalRow{{AppName: db.ChromeAppName, Seconds: 3600}}
	srv := webServer(t, fake, false)

	body := getDetail(t, srv, user.ID,
		"/activity?kind=app&name=Google+Chrome&date="+testDetailDay).Body.String()
	if !strings.Contains(body, `class="session"`) {
		t.Error("the alias record did not reach the canonical application's detail page")
	}
}

func TestActivityDetailSiteUsesTheSiteTotal(t *testing.T) {
	t.Parallel()
	fake := newFakeWeb()
	user := fake.addUser(t, "sites", testPassword)
	day := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	pageURL := "https://www.example.com/pricing"

	fake.devices = []db.DeviceRow{{ID: 7, UserID: user.ID, Name: testLaptop}}
	fake.records = []db.ActivityRecordRow{{
		DeviceID: 7, AppName: testFirefoxLower, Title: "Pricing", URL: &pageURL,
		StartedAt: day.Add(9 * time.Hour), EndedAt: day.Add(9*time.Hour + 30*time.Minute),
	}}
	fake.totals = []db.AppTotalRow{{AppName: db.FirefoxAppName, Seconds: 1800}}
	fake.sites = []db.SiteTotalRow{{Site: testSiteHost, Seconds: 1800}}
	srv := webServer(t, fake, false)

	rec := getDetail(t, srv, user.ID,
		"/activity?kind=site&name="+testSiteHost+"&date="+testDetailDay)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	for _, want := range []string{"Website", testSiteHost, "30m", `class="session"`} {
		if !strings.Contains(body, want) {
			t.Errorf("site detail page is missing %q", want)
		}
	}
}

// A subject nobody tracked is a 404, not a 500: nothing failed, the thing
// asked for does not exist in this window.
func TestActivityDetailRejectsUnknownSubjects(t *testing.T) {
	t.Parallel()
	_, srv, user := detailFixture(t)

	for _, target := range []string{
		"/activity?kind=app&name=nothing-here&date=" + testDetailDay,
		"/activity?kind=app&date=" + testDetailDay,
		"/activity?kind=nonsense&name=code&date=" + testDetailDay,
		"/activity?kind=site&name=" + testSiteHost + "&date=" + testDetailDay,
	} {
		if got := getDetail(t, srv, user.ID, target).Code; got != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", target, got)
		}
	}
}

// Every day of the week is listed, including the quiet ones: a gap is part of
// the answer to how the time was spent.
func TestActivityDetailWeekSplitsEveryDay(t *testing.T) {
	t.Parallel()
	_, srv, user := detailFixture(t)

	body := getDetail(t, srv, user.ID,
		"/activity?kind=app&name=code&view=week&date="+testDetailDay).Body.String()
	for _, want := range []string{"By day", "Mon 4", "Wed 6", "Sun 10"} {
		if !strings.Contains(body, want) {
			t.Errorf("weekly detail page is missing %q", want)
		}
	}
	if got := strings.Count(body, `class="breakdown__item"`); got < 7 {
		t.Errorf("breakdown rows = %d, want one per day of the week", got)
	}
}

func TestActivityPanelRendersWithoutChrome(t *testing.T) {
	t.Parallel()
	_, srv, user := detailFixture(t)

	rec := getDetail(t, srv, user.ID, "/activity/panel?kind=app&name=code&date="+testDetailDay)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, "<html") || strings.Contains(body, "site-header") {
		t.Error("the htmx panel rendered the full page")
	}
	if !strings.Contains(body, `class="session"`) {
		t.Error("the htmx panel is missing the session list")
	}
}

// The back link sits above the swapped panel, so changing the period has to
// rewrite it out of band. Without that, moving to another day and clicking
// back returns to the day the reader arrived on.
func TestActivityPanelUpdatesTheBackLinkOutOfBand(t *testing.T) {
	t.Parallel()
	_, srv, user := detailFixture(t)

	body := getDetail(t, srv, user.ID,
		"/activity/panel?kind=app&name=code&view=week&date=2026-05-06").Body.String()
	if !strings.Contains(body, `id="detail-back" hx-swap-oob="true"`) {
		t.Fatal("the panel does not update the back link out of band")
	}
	if !strings.Contains(body, "/?date=2026-05-06&amp;view=week") {
		t.Error("the back link does not carry the period now on screen")
	}
}

// The dashboard is where a reader clicks through from, so its rows have to
// carry the period they were looking at.
func TestDashboardRowsLinkToTheirDetailPage(t *testing.T) {
	t.Parallel()
	fake, srv, user := detailFixture(t)
	fake.sites = []db.SiteTotalRow{{Site: testSiteHost, Seconds: 900}}

	body := getDetail(t, srv, user.ID, "/?date="+testDetailDay).Body.String()
	for _, want := range []string{
		"date=2026-05-04&amp;kind=app&amp;name=code&amp;view=day",
		"date=2026-05-04&amp;kind=site&amp;name=example.com&amp;view=day",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard row does not link to %q", want)
		}
	}
}
