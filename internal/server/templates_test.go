package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/db"
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
	if got := strings.Count(html, `class="timeline__hour"`); got != 23 {
		t.Errorf("hour labels = %d, want 23 on a spring-forward day", got)
	}
}

func TestTimelineShowsTruncationNotice(t *testing.T) {
	t.Parallel()

	data := sampleTimelineData()
	data.Truncated = true
	data.RecordLimit = 5000

	html := renderPartial(t, "timeline", data)
	if !strings.Contains(html, "5000") || !strings.Contains(html, "totals below cover the whole day") {
		t.Errorf("truncation notice missing or unclear:\n%s", html)
	}
}

func TestTimelineEmptyState(t *testing.T) {
	t.Parallel()
	html := renderPartial(t, "timeline", &pageData{Chart: Chart{SpanMin: 1440}})

	if !strings.Contains(html, "Nothing tracked on this day") {
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

	return &pageData{
		User:         &db.UserRow{ID: 1, Username: "ye"},
		CSRFToken:    testCSRFValue,
		Timezone:     "UTC",
		Date:         "2026-05-04",
		Today:        "2026-05-04",
		DateLabel:    "Monday, 4 May 2026",
		Devices:      devices,
		Chart:        layout(records, devices, day),
		Totals:       []db.AppTotalRow{{AppName: testFirefoxLower, Seconds: 1800}},
		TotalSeconds: 1800,
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
