package server

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/yelinaung/trackkr/internal/db"
)

const (
	detailKindApp  = "app"
	detailKindSite = "site"

	// detailSessionsShown is how many sessions the page lists before the
	// disclosure, matched to the ten-row batches the totals use.
	detailSessionsShown = 10
	// detailSessionMax caps the list itself. A day of switching between two
	// applications produces hundreds of sessions, and a page that renders
	// every one of them stops being a summary.
	detailSessionMax = 60
	// detailTitleLimit caps the window-title breakdown.
	detailTitleLimit = 8

	// detailBarSpan is the viewBox width the breakdown bars are drawn in.
	detailBarSpan = 100.0

	// dateParam and viewParam are the filter names the dashboard and the
	// detail page both read and both link with.
	dateParam = "date"
	viewParam = "view"
)

// errDetailUnknown marks a request naming neither an application nor a site.
var errDetailUnknown = errors.New("unknown detail subject")

// handleActivityDetail serves one application or website on its own page.
func (h *webHandlers) handleActivityDetail() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := h.detailData(w, r)
		if err != nil {
			h.failDetail(w, err)
			return
		}

		if err := h.templates.renderPage(w, pageActivity, data); err != nil {
			h.fail(w, err, "rendering activity detail")
		}
	}
}

// handleActivityPanel serves the htmx partial behind the detail page's own
// date, period and device filters.
func (h *webHandlers) handleActivityPanel() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := h.detailData(w, r)
		if err != nil {
			h.failDetail(w, err)
			return
		}

		data.Partial = true
		if err := h.templates.renderPartial(w, partialDetail, data); err != nil {
			h.fail(w, err, "rendering activity detail")
		}
	}
}

// failDetail keeps a mistyped or hand-edited subject a 404 rather than a 500:
// nothing went wrong on the server, the thing asked for does not exist.
func (h *webHandlers) failDetail(w http.ResponseWriter, err error) {
	if errors.Is(err, errDetailUnknown) {
		http.Error(w, "no such application or website", http.StatusNotFound)
		return
	}
	h.fail(w, err, "building activity detail")
}

// detailData narrows one dashboard window to a single application or site.
func (h *webHandlers) detailData(w http.ResponseWriter, r *http.Request) (*pageData, error) {
	user := UserFromContext(r.Context())
	if user == nil {
		return nil, errors.New("activity detail requested without a session")
	}

	kind := r.URL.Query().Get("kind")
	name := r.URL.Query().Get("name")
	if (kind != detailKindApp && kind != detailKindSite) || name == "" {
		return nil, errDetailUnknown
	}

	token, err := h.ensureCSRF(w, r)
	if err != nil {
		return nil, err
	}
	data := h.base(r, token)
	win := h.parseWindow(r)
	h.applyWindow(data, win)

	devices, err := h.queries.ListDevicesByUser(r.Context(), user.ID)
	if err != nil {
		return nil, err
	}

	activity, err := h.queries.GetActivitySummary(r.Context(), user.ID, win.start, win.end, win.deviceID)
	if err != nil {
		return nil, err
	}

	matched := matchDetailRecords(kind, name, activity.Records)
	entry, err := h.detailEntry(r, user.ID, kind, name, activity.Totals, win)
	if err != nil {
		return nil, err
	}

	detail := &DetailView{
		Kind:      kind,
		KindLabel: detailKindLabel(kind),
		Entry:     entry,
		Seconds:   entry.Seconds,
		SharePct:  sharePercent(entry.Seconds, activity.Totals),
		Titles:    detailTitles(matched, win, entry.Fill),
		BackURL:   dashboardURL(win),
	}
	detail.Sessions, detail.SessionCount, detail.Longest = detailSessions(matched, devices, win)
	detail.SessionsShown = min(len(detail.Sessions), detailSessionsShown)
	if win.view == dashboardViewWeek {
		detail.Days = detailDays(matched, win, entry.Fill)
	}

	data.Devices = devices
	data.Detail = detail
	data.TotalSeconds = entry.Seconds
	subject := focus{records: matched, fill: entry.Fill}
	if win.view == dashboardViewWeek {
		data.Chart = layoutFocusWeek(activity.Records, subject, devices, win.start, win.end)
	} else {
		data.Chart = layoutFocusDay(activity.Records, subject, devices, win.day)
	}
	data.Truncated = activity.TimelineTruncated
	data.SourceTruncated = activity.SourceTruncated

	return data, nil
}

// detailEntry builds the header row, taking its headline number from the same
// query the dashboard row came from. Re-summing the timeline records here
// would drift from the row the reader clicked: application totals are
// computed from browser coverage rather than from the drawn slices, and the
// drawn slices are capped where the totals are not.
func (h *webHandlers) detailEntry(
	r *http.Request,
	userID int64,
	kind, name string,
	totals []db.AppTotalRow,
	win *dashboardWindow,
) (TotalView, error) {
	if kind == detailKindSite {
		sites, err := h.queries.GetSiteTotals(r.Context(), userID, win.start, win.end, win.deviceID)
		if err != nil {
			return TotalView{}, err
		}
		for _, site := range sites {
			if site.Site == name {
				return h.siteTotalView(userID, site), nil
			}
		}
		return TotalView{}, errDetailUnknown
	}

	for _, total := range totals {
		if total.AppName == name {
			return appTotalView(total, h.appIcons(r.Context(), userID, []string{name})), nil
		}
	}
	return TotalView{}, errDetailUnknown
}

// matchDetailRecords keeps the records behind one summary row. Applications
// are folded onto their canonical name exactly as the totals fold them, and
// sites are derived exactly as the site query groups them, so the detail view
// counts the same records the row was built from.
func matchDetailRecords(kind, name string, records []db.ActivityRecordRow) []db.ActivityRecordRow {
	matched := make([]db.ActivityRecordRow, 0, len(records))
	for i := range records {
		record := &records[i]
		if detailSubject(kind, record) != name {
			continue
		}
		matched = append(matched, *record)
	}
	return matched
}

func detailSubject(kind string, record *db.ActivityRecordRow) string {
	if kind == detailKindApp {
		return db.CanonicalAppName(record)
	}
	if record.URL == nil {
		return ""
	}
	site, ok := db.SiteFromURL(*record.URL)
	if !ok {
		return ""
	}
	return site
}

// detailSessions turns the matched records into the uninterrupted stretches a
// reader recognises, using the same merge the chart draws with so the list and
// the bars describe the same blocks.
func detailSessions(
	records []db.ActivityRecordRow,
	devices []db.DeviceRow,
	win *dashboardWindow,
) (sessions []SessionView, count int, longest int64) {
	names := make(map[int64]string, len(devices))
	for _, device := range devices {
		names[device.ID] = device.Name
	}

	timeFormat := "15:04"
	if win.view == dashboardViewWeek {
		timeFormat = "Mon 2 15:04"
	}

	merged := mergeAdjacentActivity(records)
	for i := range merged {
		record := &merged[i]
		from, to, ok := clampToWindow(record, win)
		if !ok {
			continue
		}

		count++
		seconds := int64(math.Round(to.Sub(from).Seconds()))
		longest = max(longest, seconds)
		if len(sessions) >= detailSessionMax {
			continue
		}
		sessions = append(sessions, SessionView{
			DeviceName: names[record.DeviceID],
			Start:      from.In(win.start.Location()).Format(timeFormat),
			End:        to.In(win.start.Location()).Format(timeFormat),
			Seconds:    seconds,
			Title:      record.Title,
		})
	}
	return sessions, count, longest
}

// detailTitles ranks the window or page titles seen within the subject. An
// application with one window all day has nothing to say here, so a single
// entry is dropped rather than repeating the header.
func detailTitles(records []db.ActivityRecordRow, win *dashboardWindow, fill string) []BreakdownView {
	seconds := make(map[string]int64)
	for i := range records {
		record := &records[i]
		from, to, ok := clampToWindow(record, win)
		if !ok || record.Title == "" {
			continue
		}
		seconds[record.Title] += int64(math.Round(to.Sub(from).Seconds()))
	}
	if len(seconds) < 2 {
		return nil
	}

	titles := make([]BreakdownView, 0, len(seconds))
	for title, total := range seconds {
		titles = append(titles, BreakdownView{Label: title, Seconds: total, Fill: fill})
	}
	sort.Slice(titles, func(a, b int) bool {
		if titles[a].Seconds != titles[b].Seconds {
			return titles[a].Seconds > titles[b].Seconds
		}
		return titles[a].Label < titles[b].Label
	})

	return scaleBreakdown(titles[:min(len(titles), detailTitleLimit)])
}

// detailDays splits a week into its days, including the empty ones: a gap in
// the week is part of the answer to how the time was spent.
func detailDays(records []db.ActivityRecordRow, win *dashboardWindow, fill string) []BreakdownView {
	var days []BreakdownView
	for day := win.start; day.Before(win.end); day = day.AddDate(0, 0, 1) {
		dayWindow := &dashboardWindow{view: win.view, start: day, end: day.AddDate(0, 0, 1)}

		var seconds int64
		for i := range records {
			from, to, ok := clampToWindow(&records[i], dayWindow)
			if !ok {
				continue
			}
			seconds += int64(math.Round(to.Sub(from).Seconds()))
		}
		days = append(days, BreakdownView{Label: day.Format("Mon 2"), Seconds: seconds, Fill: fill})
	}
	return scaleBreakdown(days)
}

// scaleBreakdown sizes each bar against the largest, so the longest fills the
// track and the rest are read against it.
func scaleBreakdown(rows []BreakdownView) []BreakdownView {
	var peak int64
	for _, row := range rows {
		peak = max(peak, row.Seconds)
	}
	if peak == 0 {
		return rows
	}
	for i := range rows {
		rows[i].Width = detailBarSpan * float64(rows[i].Seconds) / float64(peak)
	}
	return rows
}

// clampToWindow trims a record to the period on screen and reports whether
// anything of it remains.
func clampToWindow(record *db.ActivityRecordRow, win *dashboardWindow) (from, to time.Time, ok bool) {
	from = maxChartTime(record.StartedAt, win.start)
	to = minChartTime(record.EndedAt, win.end)
	if !from.Before(to) {
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

// sharePercent reports the subject's share of everything tracked in the
// window, rounded for display.
func sharePercent(seconds int64, totals []db.AppTotalRow) int {
	var tracked int64
	for _, total := range totals {
		tracked += total.Seconds
	}
	if tracked <= 0 || seconds <= 0 {
		return 0
	}
	return int(math.Round(100 * float64(seconds) / float64(tracked)))
}

// detailURL links a summary row to its own page, carrying the period the
// reader was already looking at.
func detailURL(kind, name string, win *dashboardWindow) string {
	query := url.Values{
		"kind":    {kind},
		"name":    {name},
		dateParam: {win.day.Format(dateLayout)},
		viewParam: {win.view},
	}
	if win.deviceID != nil {
		query.Set("device", strconv.FormatInt(*win.deviceID, 10))
	}
	return "/activity?" + query.Encode()
}

// dashboardURL is the way back, with the same filters still applied.
func dashboardURL(win *dashboardWindow) string {
	query := url.Values{
		dateParam: {win.day.Format(dateLayout)},
		viewParam: {win.view},
	}
	if win.deviceID != nil {
		query.Set("device", strconv.FormatInt(*win.deviceID, 10))
	}
	return "/?" + query.Encode()
}

// detailKindLabel names the subject in prose.
func detailKindLabel(kind string) string {
	if kind == detailKindSite {
		return "Website"
	}
	return "Application"
}
