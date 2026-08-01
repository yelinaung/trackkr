package server

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/yelinaung/trackkr/internal/db"
	"github.com/yelinaung/trackkr/web"
)

// pageData is what every full page render receives. Fields the base
// template touches live here so a page cannot forget them.
type pageData struct {
	User              *db.UserRow
	CSRFToken         string
	Flash             string
	FlashKind         string
	Timezone          string
	AllowRegistration bool

	// Dashboard and devices.
	Date            string
	Today           string
	DateLabel       string
	View            string
	Devices         []db.DeviceRow
	SelectedDevice  int64
	Chart           Chart
	Totals          []TotalView
	Sites           []TotalView
	TotalSeconds    int64
	Truncated       bool
	SourceTruncated bool
	RecordLimit     int
	SourceLimit     int
	CategoryTotals  []db.CategoryTotalRow

	// Category management.
	Categories        []db.CategorySummaryRow
	KnownApplications []CategoryApplicationView
	CategoryFormName  string
	CategoryFormColor string

	// Detail is set only on the single-application or single-site page.
	Detail *DetailView

	// Partial marks an htmx fragment render, which emits the
	// out-of-band heading update a full page render must not.
	Partial bool
}

// TotalView is one app or website summary row with icon fallback metadata.
type TotalView struct {
	AppName      string
	Seconds      int64
	Fill         string
	IconURL      string
	Monogram     string
	MonogramFill string
	MonogramBG   string
	// DetailURL opens this row on its own page, carrying the date, period
	// and device the reader was already looking at.
	DetailURL string
}

// CategoryApplicationView is one recently observed application and its
// current default. Assignment is nil when the application is uncategorized.
type CategoryApplicationView struct {
	AppKey       string
	AppName      string
	LastSeen     time.Time
	Assignment   *db.CategoryRow
	IconURL      string
	Monogram     string
	MonogramFill string
	MonogramBG   string
}

// DetailView is one application or website examined on its own: the same
// window the dashboard was showing, narrowed to a single subject.
type DetailView struct {
	Kind string // detailKindApp or detailKindSite.
	// KindLabel names the kind in prose: "Application" or "Website".
	KindLabel string
	Entry     TotalView
	Seconds   int64
	// SharePct is this subject's share of everything tracked in the window,
	// rounded to a whole percent for display only.
	SharePct int
	// Longest is the longest single uninterrupted session, in seconds. It
	// separates a morning of focus from the same total in scattered minutes.
	Longest int64
	// SessionCount counts every session in the window, including the ones
	// past the cap on Sessions.
	SessionCount int
	Sessions     []SessionView
	// SessionsShown is how many of Sessions the page lists before the
	// disclosure, so the summary can name the remainder.
	SessionsShown int
	// Titles ranks the window or page titles seen within the subject.
	Titles []BreakdownView
	// Days is the per-day split, filled in for the week view only.
	Days []BreakdownView
	// BackURL returns to the dashboard with the same filters.
	BackURL         string
	Categories      []db.CategorySummaryRow
	EditableRecords []EditableRecordView
	RecordsNextURL  string
	RecordReturnURL string
}

// EditableRecordView presents one raw activity row. The editor does not use
// merged sessions, because mutations must identify the stored record exactly.
type EditableRecordView struct {
	ID           int64
	DeviceName   string
	Start        string
	End          string
	Title        string
	CategoryName string
	HasOverride  bool
	CategoryID   *int64
}

// SessionView is one uninterrupted stretch of use.
type SessionView struct {
	DeviceName string
	Start      string
	End        string
	Seconds    int64
	Title      string
}

// BreakdownView is one labelled bar in a ranked split: a day of the week, or
// a window title within one application.
type BreakdownView struct {
	Label   string
	Seconds int64
	// Width is the bar length in a 0-100 viewBox. Geometry travels as an SVG
	// attribute rather than inline CSS, the same as the timeline bars, so the
	// page needs no style-src concession.
	Width float64
	Fill  string
}

// templates holds one parsed set per page, plus the standalone partials.
// Parsing happens once at startup; a broken template is a boot failure,
// not a 500 on first request.
type templates struct {
	pages    map[string]*template.Template
	partials map[string]*template.Template
}

const (
	pageLogin      = "login"
	pageRegister   = "register"
	pageDashboard  = "dashboard"
	pageDevices    = "devices"
	pageActivity   = "activity"
	pageCategories = "categories"
)

const (
	partialTimeline = "timeline"
	partialDetail   = "activity_detail"
	partialDevices  = "device_rows"
)

var templateFuncs = template.FuncMap{
	"duration":       humanDuration,
	"date":           func(t time.Time) string { return t.Format("2 Jan 2006") },
	"nextTotalCount": nextTotalCount,
	"sub":            func(a, b int) int { return a - b },
	"categoryColors": categoryColorOptions,
	"sameCategory":   sameCategory,
}

func sameCategory(id *int64, candidate int64) bool {
	return id != nil && *id == candidate
}

func nextTotalCount(total int) int {
	return min(max(total-dashboardTotalPageSize, 0), dashboardTotalPageSize)
}

// parseTemplates builds a set per page: base plus that page plus the
// partials it embeds. Pages are parsed separately so two pages can both
// define "content" without colliding.
func parseTemplates() (*templates, error) {
	const (
		periodFile   = "templates/partials/period.html"
		timelineFile = "templates/partials/timeline.html"
		detailFile   = "templates/partials/activity_detail.html"
		devicesFile  = "templates/partials/device_rows.html"
	)

	// A partial is a set rather than a file: the timeline and the detail
	// page both draw the chart, so "chart" is parsed into both.
	partialFiles := map[string][]string{
		partialTimeline: {timelineFile, periodFile},
		partialDetail:   {detailFile, periodFile},
		partialDevices:  {devicesFile},
	}

	pageFiles := map[string][]string{
		pageLogin:      {"templates/login.html"},
		pageRegister:   {"templates/register.html"},
		pageDashboard:  {"templates/dashboard.html", timelineFile, periodFile},
		pageDevices:    {"templates/devices.html", devicesFile},
		pageActivity:   {"templates/activity.html", detailFile, periodFile},
		pageCategories: {"templates/categories.html"},
	}

	t := &templates{
		pages:    make(map[string]*template.Template, len(pageFiles)),
		partials: make(map[string]*template.Template, len(partialFiles)),
	}

	for name, files := range pageFiles {
		set := template.New(name).Funcs(templateFuncs)
		all := append([]string{"templates/base.html"}, files...)

		parsed, err := set.ParseFS(web.Templates, all...)
		if err != nil {
			return nil, fmt.Errorf("parsing page %s: %w", name, err)
		}
		t.pages[name] = parsed
	}

	for name, files := range partialFiles {
		set := template.New("partial").Funcs(templateFuncs)

		parsed, err := set.ParseFS(web.Templates, files...)
		if err != nil {
			return nil, fmt.Errorf("parsing partial %s: %w", name, err)
		}
		t.partials[name] = parsed
	}

	return t, nil
}

// renderPage writes a full page, buffering first so a template error
// cannot emit half a document with a 200 already sent.
func (t *templates) renderPage(w http.ResponseWriter, name string, data *pageData) error {
	set, ok := t.pages[name]
	if !ok {
		return fmt.Errorf("unknown page %q", name)
	}

	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, "base", data); err != nil {
		return fmt.Errorf("rendering page %s: %w", name, err)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := buf.WriteTo(w)
	return err
}

// renderPartial writes an htmx fragment.
func (t *templates) renderPartial(w http.ResponseWriter, name string, data *pageData) error {
	set, ok := t.partials[name]
	if !ok {
		return fmt.Errorf("unknown partial %q", name)
	}

	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, name, data); err != nil {
		return fmt.Errorf("rendering partial %s: %w", name, err)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := buf.WriteTo(w)
	return err
}

// humanDuration renders seconds the way a person reads a day: "3h 12m",
// "48m", "31s".
func humanDuration(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}
