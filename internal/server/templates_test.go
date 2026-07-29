package server

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/db"
	"github.com/yelinaung/trackkr/web"
)

func mustTemplates(t *testing.T) *templates {
	t.Helper()
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	return tmpl
}

func renderPage(t *testing.T, name string, data *pageData) string {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := mustTemplates(t).renderPage(rec, name, data); err != nil {
		t.Fatalf("renderPage(%s): %v", name, err)
	}
	return rec.Body.String()
}

func renderPartial(t *testing.T, name string, data *pageData) string {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := mustTemplates(t).renderPartial(rec, name, data); err != nil {
		t.Fatalf("renderPartial(%s): %v", name, err)
	}
	return rec.Body.String()
}

// A template that fails to parse must be a boot failure, so this is the
// test that catches a broken page before a user does.
func TestAllTemplatesParse(t *testing.T) {
	t.Parallel()
	tmpl := mustTemplates(t)

	for _, page := range []string{pageLogin, pageRegister, pageDashboard, pageDevices} {
		if _, ok := tmpl.pages[page]; !ok {
			t.Errorf("page %q missing", page)
		}
	}
	for _, partial := range []string{"timeline", "device_rows"} {
		if _, ok := tmpl.partials[partial]; !ok {
			t.Errorf("partial %q missing", partial)
		}
	}
}

// The whole CSP design rests on no page emitting inline CSS, so assert
// it rather than trusting review. A style attribute or a <style> block
// would be silently dropped by the browser under style-src 'self'.
func TestNoTemplateEmitsInlineCSS(t *testing.T) {
	t.Parallel()

	rendered := map[string]string{
		pageLogin:     renderPage(t, pageLogin, &pageData{CSRFToken: testCSRFValue}),
		pageRegister:  renderPage(t, pageRegister, &pageData{CSRFToken: testCSRFValue}),
		pageDashboard: renderPage(t, pageDashboard, sampleTimelineData()),
		pageDevices:   renderPage(t, pageDevices, sampleDevicesData()),
		"timeline":    renderPartial(t, "timeline", sampleTimelineData()),
		"device_rows": renderPartial(t, "device_rows", sampleDevicesData()),
	}

	for name, html := range rendered {
		if strings.Contains(html, "style=\"") {
			t.Errorf("%s emits a style attribute; geometry must travel as SVG attributes", name)
		}
		if strings.Contains(html, "<style") {
			t.Errorf("%s emits a <style> block, which style-src 'self' rejects", name)
		}
	}
}

// htmx injects its own indicator styles unless configured otherwise,
// which would violate the CSP on every page that loads htmx.
func TestBaseDisablesHTMXIndicatorStyles(t *testing.T) {
	t.Parallel()
	html := renderPage(t, pageLogin, &pageData{CSRFToken: testCSRFValue})

	if !strings.Contains(html, `name="htmx-config"`) {
		t.Fatal("base template is missing the htmx-config meta tag")
	}
	if !strings.Contains(html, `&#34;includeIndicatorStyles&#34;:false`) &&
		!strings.Contains(html, `"includeIndicatorStyles":false`) {
		t.Error("htmx-config does not disable includeIndicatorStyles")
	}
}

func TestPagesLoadLocalAssetsOnly(t *testing.T) {
	t.Parallel()
	html := renderPage(t, pageLogin, &pageData{CSRFToken: testCSRFValue})

	for _, want := range []string{
		`href="/static/bootstrap.min.css"`,
		`href="/static/style.css"`,
		`src="/static/htmx.min.js"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("login page missing %s", want)
		}
	}
	// A CDN reference would be blocked by the CSP at runtime.
	for _, bad := range []string{"https://cdn", "unpkg.com", "jsdelivr"} {
		if strings.Contains(html, bad) {
			t.Errorf("login page references external host %q", bad)
		}
	}
}

func TestFormsCarryCSRFToken(t *testing.T) {
	t.Parallel()

	for _, page := range []string{pageLogin, pageRegister} {
		html := renderPage(t, page, &pageData{CSRFToken: "tok-123"})
		if !strings.Contains(html, `name="csrf_token" value="tok-123"`) {
			t.Errorf("%s form is missing the CSRF field", page)
		}
	}
}

func TestTotalsUseIconsWithoutRedundantSwatches(t *testing.T) {
	t.Parallel()
	html := renderPartial(t, "timeline", sampleTimelineData())

	if strings.Contains(html, "totals__swatch") {
		t.Error("redundant colour swatch is still rendered beside an icon")
	}
	if !strings.Contains(html, "totals__icon") {
		t.Error("total rows have no icon or monogram fallback")
	}
	if strings.Contains(html, "data-app=") {
		t.Error("dead data-app attribute is still emitted")
	}
}

func TestTimelineRendersBarsAsSVGAttributes(t *testing.T) {
	t.Parallel()
	html := renderPartial(t, "timeline", sampleTimelineData())

	if !strings.Contains(html, "<svg") {
		t.Fatal("timeline has no svg element")
	}
	if !strings.Contains(html, `x="60"`) || !strings.Contains(html, `width="30"`) {
		t.Errorf("bar geometry missing from output:\n%s", html)
	}
	if !strings.Contains(html, "<title>") {
		t.Error("bars have no <title>, so there is no accessible tooltip")
	}
	if !strings.Contains(html, "bar--0") {
		t.Error("stagger bucket class missing; the reveal needs it")
	}
}

func TestTimelineHourMarksMatchDaySpan(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	// Spring forward: a 23-hour day must render 23 hour labels.
	data := sampleTimelineData()
	data.Chart = layout(nil, nil, time.Date(2026, 3, 8, 12, 0, 0, 0, loc))
	data.Chart.Lanes = sampleTimelineData().Chart.Lanes

	html := renderPartial(t, "timeline", data)
	if got := strings.Count(html, `class="timeline__hour timeline__hour--1"`); got != 23 {
		t.Errorf("hour labels = %d, want 23 on a spring-forward day", got)
	}
}

func TestDashboardViewSwitchReflectsSelection(t *testing.T) {
	t.Parallel()

	data := sampleTimelineData()
	data.View = dashboardViewWeek
	html := renderPage(t, pageDashboard, data)

	for _, want := range []string{
		`name="view" type="radio" value="day"`,
		`name="view" type="radio" value="week" checked`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard view switch is missing %q", want)
		}
	}
}

// Every browser record is app_name "Firefox", so an app breakdown alone
// cannot answer where the browsing time went.
func TestTimelineListsSitesSeparatelyFromApps(t *testing.T) {
	t.Parallel()
	html := renderPartial(t, "timeline", sampleTimelineData())

	if !strings.Contains(html, `class="totals-grid"`) {
		t.Error("app and website summaries do not share the two-column layout")
	}
	if !strings.Contains(html, "Websites") {
		t.Error("no per-site section rendered")
	}
	for _, want := range []string{"youtube.com", "github.com", "20m", "10m"} {
		if !strings.Contains(html, want) {
			t.Errorf("site list is missing %q", want)
		}
	}
	// The two sections are distinct: apps still summarise the day.
	if !strings.Contains(html, "Applications") {
		t.Error("the app summary disappeared")
	}
}

func TestTimelineSiteMonogramFallback(t *testing.T) {
	t.Parallel()

	data := sampleTimelineData()
	data.Sites = []TotalView{{
		AppName:      testHost,
		Seconds:      60,
		Fill:         "hsl(20 62% 48%)",
		Monogram:     "12",
		MonogramFill: monogramDark,
	}}
	html := renderPartial(t, "timeline", data)

	if strings.Contains(html, `src=""`) {
		t.Error("site fallback emits an empty image source")
	}
	if !strings.Contains(html, `class="totals__icon totals__monogram"`) ||
		!strings.Contains(html, `fill="`+monogramDark+`">12</text>`) {
		t.Errorf("timeline lacks the site monogram fallback:\n%s", html)
	}
}

// A day with no browsing must not render an empty section.
//
// Both shapes are covered on purpose: the handler builds the slice with
// make(...), so it hands the template an empty *non-nil* slice on a day
// of desktop-only activity. Go template truth for a slice is len > 0,
// not nil-ness, so both are falsy -- but only a test says so out loud.
func TestTimelineOmitsSitesWhenThereAreNone(t *testing.T) {
	t.Parallel()

	for name, sites := range map[string][]TotalView{
		"nil":           nil,
		"empty non-nil": make([]TotalView, 0, 4),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			data := sampleTimelineData()
			data.Sites = sites

			if strings.Contains(renderPartial(t, "timeline", data), "Websites") {
				t.Errorf("%s site list rendered an empty section", name)
			}
		})
	}
}

func TestTimelineShowsTruncationNotice(t *testing.T) {
	t.Parallel()

	data := sampleTimelineData()
	data.Truncated = true
	data.RecordLimit = 5000

	html := renderPartial(t, "timeline", data)
	if !strings.Contains(html, "5000") || !strings.Contains(html, "totals below cover the selected range") {
		t.Errorf("truncation notice missing or unclear:\n%s", html)
	}
}

func TestTimelineShowsSourceTruncationNotice(t *testing.T) {
	t.Parallel()

	data := sampleTimelineData()
	data.Truncated = true
	data.SourceTruncated = true
	data.SourceLimit = db.ActivitySourceLimit

	html := renderPartial(t, "timeline", data)
	if !strings.Contains(html, "25000 source records") ||
		!strings.Contains(html, "totals show") {
		t.Errorf("source truncation notice missing or unclear:\n%s", html)
	}
	if strings.Contains(html, "totals below cover the selected range") {
		t.Error("source-truncated totals are described as complete")
	}
}

func TestTimelineEmptyState(t *testing.T) {
	t.Parallel()
	html := renderPartial(t, "timeline", &pageData{Chart: Chart{SpanMin: 1440}})

	if !strings.Contains(html, "Nothing tracked in this period") {
		t.Errorf("empty state missing:\n%s", html)
	}
}

// The dashboard shows keys deliberately: it is reached with a password,
// unlike the API, which must never expose them.
func TestDeviceRowsShowKeyAndCSRFHeader(t *testing.T) {
	t.Parallel()
	html := renderPartial(t, "device_rows", sampleDevicesData())

	if !strings.Contains(html, "secret-key-value") {
		t.Error("device key not shown on the management page")
	}
	if !strings.Contains(html, "hx-delete=\"/devices/7\"") {
		t.Error("delete control missing")
	}
	if !strings.Contains(html, "X-CSRF-Token") {
		t.Error("hx-delete does not send the CSRF header, so it would 403")
	}
	if !strings.Contains(html, "hx-confirm") {
		t.Error("delete has no confirmation; there are no Bootstrap modals to fall back on")
	}
}

func TestHumanDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		seconds int64
		want    string
	}{
		{0, "0s"},
		{45, "45s"},
		{60, "1m"},
		{599, "9m"},
		{3600, "1h 00m"},
		{5415, "1h 30m"},
		{86400, "24h 00m"},
	}

	for _, tt := range tests {
		if got := humanDuration(tt.seconds); got != tt.want {
			t.Errorf("humanDuration(%d) = %q, want %q", tt.seconds, got, tt.want)
		}
	}
}

func sampleTimelineData() *pageData {
	day := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	records := []db.ActivityRecordRow{{
		DeviceID:  7,
		AppName:   testFirefoxLower,
		Title:     "docs",
		StartedAt: day.Add(time.Hour),
		EndedAt:   day.Add(90 * time.Minute),
	}}
	devices := []db.DeviceRow{{ID: 7, Name: testLaptop}}

	fill, monogramFill := appPalette(testFirefoxLower)
	return &pageData{
		User:      &db.UserRow{ID: 1, Username: "ye"},
		CSRFToken: testCSRFValue,
		Timezone:  "UTC",
		Date:      "2026-05-04",
		Today:     "2026-05-04",
		DateLabel: "Monday, 4 May 2026",
		View:      dashboardViewDay,
		Devices:   devices,
		Chart:     layout(records, devices, day),
		Totals: []TotalView{{
			AppName: testFirefoxLower, Seconds: 1800,
			Fill: fill, Monogram: "FI", MonogramFill: monogramFill,
		}},
		Sites: []TotalView{
			{AppName: "youtube.com", Seconds: 1200, Fill: appColor("youtube.com"), IconURL: "/site-icons/youtube.com?sig=test"},
			{AppName: "github.com", Seconds: 600, Fill: appColor("github.com"), IconURL: "/site-icons/github.com?sig=test"},
		},
		TotalSeconds: 1800,
	}
}

func TestTimelineAppIconAndMonogramFallback(t *testing.T) {
	t.Parallel()

	data := sampleTimelineData()
	fallback := renderPartial(t, "timeline", data)
	if !strings.Contains(fallback, `class="totals__icon totals__monogram"`) ||
		!strings.Contains(fallback, `fill="`+data.Totals[0].MonogramFill+`">FI</text>`) {
		t.Errorf("timeline lacks deterministic monogram fallback:\n%s", fallback)
	}

	data = sampleTimelineData()
	data.Totals[0].IconURL = "/app-icons/1/abc.png"
	withIcon := renderPartial(t, "timeline", data)
	for _, want := range []string{
		`src="/app-icons/1/abc.png"`,
		`width="22"`,
		`height="22"`,
		`alt=""`,
	} {
		if !strings.Contains(withIcon, want) {
			t.Errorf("timeline icon lacks %q:\n%s", want, withIcon)
		}
	}
	if strings.Contains(withIcon, "totals__monogram") {
		t.Error("timeline rendered a fallback beside a real icon")
	}
}

func sampleDevicesData() *pageData {
	return &pageData{
		User:      &db.UserRow{ID: 1, Username: "ye"},
		CSRFToken: testCSRFValue,
		Timezone:  "UTC",
		Devices: []db.DeviceRow{{
			ID:         7,
			UserID:     1,
			Name:       testLaptop,
			DeviceType: testDesktop,
			APIKey:     "secret-key-value",
			CreatedAt:  time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
		}},
	}
}

// The date heading lives outside the swap target, so a filter change
// would leave it naming the previous day without an out-of-band update.
func TestTimelinePartialUpdatesHeadingOutOfBand(t *testing.T) {
	t.Parallel()

	data := sampleTimelineData()
	data.Partial = true
	html := renderPartial(t, "timeline", data)

	if !strings.Contains(html, `hx-swap-oob="true"`) {
		t.Error("partial does not update the heading out of band")
	}
	if !strings.Contains(html, `id="page-title"`) {
		t.Error("out-of-band heading has no matching id")
	}
	if !strings.Contains(html, data.DateLabel) {
		t.Error("out-of-band heading does not carry the selected date")
	}
}

// The full page emits the heading itself; repeating it in the embedded
// partial would render the date twice.
func TestFullPageRenderHasOneHeading(t *testing.T) {
	t.Parallel()
	html := renderPage(t, pageDashboard, sampleTimelineData())

	if got := strings.Count(html, `id="page-title"`); got != 1 {
		t.Errorf("page-title appears %d times, want 1", got)
	}
	if strings.Contains(html, "hx-swap-oob") {
		t.Error("full page render emitted an out-of-band swap")
	}
}

// Every asset the stylesheet pulls must be local, or font-src 'self'
// silently drops it and the page falls back to system fonts.
func TestStylesheetReferencesOnlyLocalAssets(t *testing.T) {
	t.Parallel()

	css, err := web.Static.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("reading style.css: %v", err)
	}

	refs := regexp.MustCompile(`url\(["']?([^"')]+)`).FindAllStringSubmatch(string(css), -1)
	if len(refs) == 0 {
		t.Fatal("no url() references found; the test is not looking at the right file")
	}
	for _, m := range refs {
		if !strings.HasPrefix(m[1], "/static/") {
			t.Errorf("stylesheet references %q, which is not served from /static/", m[1])
		}
	}
}

// The fonts named in the tokens must be the ones actually vendored.
func TestVendoredFontsMatchStylesheet(t *testing.T) {
	t.Parallel()

	css, err := web.Static.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("reading style.css: %v", err)
	}

	for _, want := range []string{
		"static/fonts/inter-latin-variable.woff2",
		"static/fonts/ibm-plex-mono-latin-400-normal.woff2",
	} {
		if _, err := web.Static.ReadFile(want); err != nil {
			t.Errorf("%s is referenced but not embedded: %v", want, err)
		}
		if !strings.Contains(string(css), "/"+want) {
			t.Errorf("%s is embedded but not referenced by the stylesheet", want)
		}
	}
}

// HTML minlength counts UTF-16 code units while the server counts
// Unicode characters, so six emoji pass in the browser and fail on the
// server. The form must not carry a second, differently-defined rule.
func TestRegisterFormHasNoClientLengthRule(t *testing.T) {
	t.Parallel()
	html := renderPage(t, pageRegister, &pageData{CSRFToken: testCSRFValue})

	if strings.Contains(html, "minlength") {
		t.Error("register form has a minlength attribute that disagrees with the server")
	}
	if !strings.Contains(html, "At least 12 characters") {
		t.Error("the policy is no longer stated to the visitor")
	}
}
