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
}

// templates holds one parsed set per page, plus the standalone partials.
// Parsing happens once at startup; a broken template is a boot failure,
// not a 500 on first request.
type templates struct {
	pages    map[string]*template.Template
	partials map[string]*template.Template
}

const (
	pageLogin     = "login"
	pageRegister  = "register"
	pageDashboard = "dashboard"
	pageDevices   = "devices"
)

var templateFuncs = template.FuncMap{
	"duration":       humanDuration,
	"date":           func(t time.Time) string { return t.Format("2 Jan 2006") },
	"nextTotalCount": nextTotalCount,
}

func nextTotalCount(total int) int {
	return min(max(total-dashboardTotalPageSize, 0), dashboardTotalPageSize)
}

// parseTemplates builds a set per page: base plus that page plus the
// partials it embeds. Pages are parsed separately so two pages can both
// define "content" without colliding.
func parseTemplates() (*templates, error) {
	partialFiles := []string{
		"templates/partials/timeline.html",
		"templates/partials/device_rows.html",
	}

	pageFiles := map[string][]string{
		pageLogin:     {"templates/login.html"},
		pageRegister:  {"templates/register.html"},
		pageDashboard: {"templates/dashboard.html", "templates/partials/timeline.html"},
		pageDevices:   {"templates/devices.html", "templates/partials/device_rows.html"},
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

	for _, file := range partialFiles {
		set := template.New("partial").Funcs(templateFuncs)

		parsed, err := set.ParseFS(web.Templates, file)
		if err != nil {
			return nil, fmt.Errorf("parsing partial %s: %w", file, err)
		}
		// Key on the defined name: "timeline", "device_rows".
		t.partials[partialName(file)] = parsed
	}

	return t, nil
}

func partialName(path string) string {
	base := path[len("templates/partials/") : len(path)-len(".html")]
	return base
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
