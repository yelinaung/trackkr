package server

import (
	"fmt"
	"hash/fnv"
	"math"
	"time"

	"github.com/yelinaung/trackkr/internal/db"
)

// minBarFraction floors a bar's width as a fraction of the visible range
// rather than as a fixed number of minutes.
//
// A one-minute floor is meaningless on screen: the chart is 1440 units
// wide, so at its minimum rendered width a minute is under half a
// pixel and a real visit is invisible. This is a visibility affordance,
// not a claim about duration -- the tooltip carries the true range, and
// the totals below the chart are computed from the data, not the bars.
const (
	minBarFraction = 1.0 / 480.0
	monogramDark   = "#000000"
	monogramLight  = "#ffffff"

	// The chip palette, kept as integers so the CSS string and the
	// contrast calculation cannot drift apart.
	monogramSaturation = 32
	monogramLightness  = 68
)

// Bar is one activity block, positioned in minutes from the visible range.
// Geometry travels to the template as SVG presentation attributes,
// never as inline CSS, so the page needs no style-src concession.
type Bar struct {
	X         float64
	Width     float64
	Fill      string
	AppName   string
	Title     string
	TimeRange string
	Bucket    int // 0-11, drives the CSS stagger without a per-bar delay.
	// Muted marks a bar drawn only as context, on a chart focused on one
	// application or site. It keeps the day's shape visible behind the
	// subject without competing with it.
	Muted bool
}

// Lane is one device's row of bars.
type Lane struct {
	DeviceID   int64
	DeviceName string
	Bars       []Bar
}

// Chart is everything the timeline partial needs to render.
//
// The window is the drawn range, not the day: empty hours at either end
// are trimmed, so a day of afternoon work does not spend two thirds of
// the chart proving that the morning was quiet.
type Chart struct {
	WindowStart time.Time
	WindowEnd   time.Time
	SpanMin     float64
	HourMarks   []HourMark
	Lanes       []Lane
	// Compact drops the chart's minimum drawing width so a short axis fits
	// the page instead of scrolling. A day is worth scrolling through; a
	// rolling window is not, and scrolling it would push the newest
	// activity -- the reason to ask for the last hour at all -- off the
	// right edge.
	Compact bool
}

// HourMark labels one time cell of the axis. The axis is HTML rather
// than SVG text, which would be distorted by the non-uniform scaling the
// bars need. SpanHours keeps DST-short or DST-long days aligned in a week.
type HourMark struct {
	Label     string
	SpanHours int
	// GapBefore marks a cell whose predecessor is not the hour before it,
	// because the quiet hours between them were dropped. The chart has to
	// say so: without a break the eye reads adjacency as continuity.
	GapBefore bool
}

// dayBounds returns the wall-clock day containing t in its own location.
// The end is derived with AddDate rather than Add(24h): on a DST
// transition the local day is 23 or 25 hours long, and a fixed 24-hour
// span would slide the whole chart against the clock.
func dayBounds(day time.Time) (start, end time.Time) {
	loc := day.Location()
	start = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	end = start.AddDate(0, 0, 1)
	return start, end
}

// minChartWindow floors how much of the day the chart draws.
//
// Without it a morning of light activity zooms to an hour or two, and
// the scale then shifts under the reader every few minutes as records
// arrive. Six hours is wide enough that a day fills in rather than
// redraws.
const minChartWindow = 6 * time.Hour

// chartWindow narrows the drawn range to the hours holding activity,
// padded by one hour on each side so the first bar does not sit flush
// against the edge, where a reader cannot tell a quiet hour from a cut.
//
// Only the ends are trimmed. Dropping an empty stretch from the middle
// would put a bar ending at 11:58 beside one starting at 15:04, and the
// eye reads adjacency as continuity; it would also make the same
// distance mean different amounts of time in different places, which is
// the one thing a timeline offers that a list does not.
func chartWindow(records []db.ActivityRecordRow, dayStart, dayEnd time.Time) (time.Time, time.Time) {
	first, last, ok := recordExtent(records, dayStart, dayEnd)
	if !ok {
		return dayStart, dayEnd
	}

	start := floorHour(first).Add(-time.Hour)
	end := ceilHour(last).Add(time.Hour)

	// Whole hours either way, so the axis cells keep landing on the
	// clock: the deficit is a whole number of hours and its halves are
	// rounded back to one.
	if deficit := minChartWindow - end.Sub(start); deficit > 0 {
		back := (deficit / 2).Round(time.Hour)
		start = start.Add(-back)
		end = end.Add(deficit - back)
	}

	// Spend whatever a clamp takes on the opposite side, so activity at
	// either edge of the day still gets the full window.
	if start.Before(dayStart) {
		end = end.Add(dayStart.Sub(start))
		start = dayStart
	}
	if end.After(dayEnd) {
		start = start.Add(dayEnd.Sub(end))
		end = dayEnd
	}
	if start.Before(dayStart) {
		start = dayStart
	}
	return start, end
}

// recordExtent reports the earliest start and latest end among records
// that overlap the day, each already clamped to it.
func recordExtent(records []db.ActivityRecordRow, dayStart, dayEnd time.Time) (first, last time.Time, ok bool) {
	for i := range records {
		rec := &records[i]
		from := rec.StartedAt
		if from.Before(dayStart) {
			from = dayStart
		}
		to := rec.EndedAt
		if to.After(dayEnd) {
			to = dayEnd
		}
		if !to.After(from) {
			continue
		}
		if !ok || from.Before(first) {
			first = from
		}
		if !ok || to.After(last) {
			last = to
		}
		ok = true
	}
	return first, last, ok
}

// floorHour and ceilHour work on the wall clock rather than by
// truncating absolute time, which would land off the hour in a zone
// offset by thirty or forty-five minutes.
func floorHour(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location())
}

func ceilHour(t time.Time) time.Time {
	floor := floorHour(t)
	if t.Equal(floor) {
		return floor
	}
	return floor.Add(time.Hour)
}

// mergeAdjacentActivity collapses a run of touching records for the same
// application on one device into a single bar.
//
// A three-second poll turns an hour in one application into hundreds of
// consecutive records, and deduplication then carves the desktop ones around
// each browser observation. Drawn one rectangle apiece they all land at the
// minimum bar width and the lane reads as noise -- the chart becomes the least
// legible thing on a page whose lists are perfectly clear. Merging is honest
// here because the runs really are contiguous: the merged bar covers exactly
// the same span as the records it replaces.
//
// Records arrive sorted by (StartedAt, DeviceID, ID), so the previous bar for
// a device is the only merge candidate.
func mergeAdjacentActivity(records []db.ActivityRecordRow) []db.ActivityRecordRow {
	merged := make([]db.ActivityRecordRow, 0, len(records))
	lastByDevice := make(map[int64]int, 4)

	for i := range records {
		record := records[i]
		if index, ok := lastByDevice[record.DeviceID]; ok {
			previous := &merged[index]
			// Touching or overlapping, never bridging a gap: a pause
			// belongs on the chart.
			if previous.AppName == record.AppName && !record.StartedAt.After(previous.EndedAt) {
				if record.EndedAt.After(previous.EndedAt) {
					previous.EndedAt = record.EndedAt
				}
				previous.DurationS = int(previous.EndedAt.Sub(previous.StartedAt).Seconds())
				// The run spans several titles, so naming one of them in
				// the tooltip would be picking a winner arbitrarily.
				if previous.Title != record.Title {
					previous.Title = ""
				}
				continue
			}
		}
		merged = append(merged, record)
		lastByDevice[record.DeviceID] = len(merged) - 1
	}
	return merged
}

// activeHourScale drops hours no record touches, so a day with a quiet
// afternoon spends its width on the hours that were worked.
//
// Bars keep their proportions. An hour survives when any record overlaps it,
// so every hour a record touches survives, and the hours one record spans are
// consecutive both in real time and in the compressed axis. A bar's width is
// therefore still exactly its duration -- only empty stretches disappear.
//
// What is lost is adjacency: a bar ending at 12:58 can sit beside one
// starting at 19:04. Every cell after a dropped run carries GapBefore so the
// axis can draw the break rather than imply continuity.
type activeHourScale struct {
	// position[i] is the compressed index of absolute hour i, or -1 when
	// that hour was dropped.
	position []int
	kept     int
}

func newActiveHourScale(records []db.ActivityRecordRow, start, end time.Time) *activeHourScale {
	total := int(end.Sub(start) / time.Hour)
	if total <= 0 {
		return nil
	}

	active := make([]bool, total)
	for i := range records {
		record := &records[i]
		from := maxChartTime(record.StartedAt, start)
		to := minChartTime(record.EndedAt, end)
		if !from.Before(to) {
			continue
		}
		first := int(from.Sub(start) / time.Hour)
		// An interval ending exactly on the hour does not reach into it.
		last := int(to.Sub(start).Nanoseconds()-1) / int(time.Hour)
		for h := max(first, 0); h <= min(last, total-1); h++ {
			active[h] = true
		}
	}

	scale := &activeHourScale{position: make([]int, total)}
	for h := range active {
		if active[h] {
			scale.position[h] = scale.kept
			scale.kept++
			continue
		}
		scale.position[h] = -1
	}
	if scale.kept == 0 || scale.kept == total {
		return nil // nothing to drop; keep the plain linear axis
	}
	return scale
}

// minutesAt maps a time onto the compressed axis. A time inside a dropped
// hour cannot occur for a record, but clamps to the segment edge if it does.
func (s *activeHourScale) minutesAt(t, start time.Time) float64 {
	offset := t.Sub(start)
	hour := int(offset / time.Hour)
	if hour >= len(s.position) {
		return float64(s.kept) * 60
	}
	if hour < 0 {
		return 0
	}
	if s.position[hour] < 0 {
		// Round to the nearest kept edge rather than inventing a position.
		for h := hour; h >= 0; h-- {
			if s.position[h] >= 0 {
				return float64(s.position[h]+1) * 60
			}
		}
		return 0
	}
	within := offset - time.Duration(hour)*time.Hour
	return float64(s.position[hour])*60 + within.Minutes()
}

func (s *activeHourScale) marks(start time.Time) []HourMark {
	marks := make([]HourMark, 0, s.kept)
	gap := false
	for h, position := range s.position {
		if position < 0 {
			gap = true
			continue
		}
		marks = append(marks, HourMark{
			Label:     start.Add(time.Duration(h) * time.Hour).Format("15"),
			SpanHours: 1,
			GapBefore: gap && len(marks) > 0,
		})
		gap = false
	}
	return marks
}

func maxChartTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minChartTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// layout converts records into positioned bars for one day. Records are
// clamped to the drawn window, which is what the overlapping database
// query makes possible for spans crossing midnight.
func layout(records []db.ActivityRecordRow, devices []db.DeviceRow, day time.Time) Chart {
	dayStart, dayEnd := dayBounds(day)
	start, end := chartWindow(records, dayStart, dayEnd)
	merged := mergeAdjacentActivity(records)
	if scale := newActiveHourScale(merged, start, end); scale != nil {
		return layoutScaled(merged, devices, start, end, scale.marks(start), false, scale)
	}
	return layoutRange(records, devices, start, end, hourMarks(start, end), false)
}

// layoutWeek draws a fixed calendar week with one proportional axis cell per
// local day. Unlike a day chart, it does not trim quiet days from either end.
func layoutWeek(
	records []db.ActivityRecordRow,
	devices []db.DeviceRow,
	start, end time.Time,
) Chart {
	return layoutRange(records, devices, start, end, dayMarks(start, end), true)
}

func layoutRange(
	records []db.ActivityRecordRow,
	devices []db.DeviceRow,
	start, end time.Time,
	marks []HourMark,
	showDates bool,
) Chart {
	return layoutScaled(mergeAdjacentActivity(records), devices, start, end, marks, showDates, nil)
}

// layoutScaled places bars on either the plain linear axis (scale nil) or a
// compressed one that has dropped its empty hours.
func layoutScaled(
	records []db.ActivityRecordRow,
	devices []db.DeviceRow,
	start, end time.Time,
	marks []HourMark,
	showDates bool,
	scale *activeHourScale,
) Chart {
	return layoutFocused(nil, focus{records: records}, devices, start, end, marks, showDates, scale)
}

// focus is one subject drawn against the rest of a period: the records that
// belong to it, and the colour the page identifies it by.
//
// The fill is the subject's, not each record's. A site is observed through a
// browser, so colouring its bars by application name would paint
// news.ycombinator.com in Chrome's hue while the legend beside the chart
// showed the site's -- and the same split happens to a browser stored under a
// platform alias.
type focus struct {
	records []db.ActivityRecordRow
	fill    string
}

// layoutFocusDay draws one day with focus highlighted against all as context.
// The window and the compressed axis come from the full set, so the detail
// chart lines up with the dashboard the reader arrived from.
func layoutFocusDay(all []db.ActivityRecordRow, subject focus, devices []db.DeviceRow, day time.Time) Chart {
	dayStart, dayEnd := dayBounds(day)
	start, end := chartWindow(all, dayStart, dayEnd)
	context := mergeAdjacentActivity(all)
	merged := focus{records: mergeAdjacentActivity(subject.records), fill: subject.fill}
	if scale := newActiveHourScale(context, start, end); scale != nil {
		return layoutFocused(context, merged, devices, start, end, scale.marks(start), false, scale)
	}
	return layoutFocused(context, merged, devices, start, end, hourMarks(start, end), false, nil)
}

// layoutFocusWeek is layoutFocusDay over a fixed calendar week.
func layoutFocusWeek(
	all []db.ActivityRecordRow,
	subject focus,
	devices []db.DeviceRow,
	start, end time.Time,
) Chart {
	return layoutFocusRangeWith(all, subject, devices, start, end, dayMarks(start, end), true)
}

// layoutFocusRange is layoutFocusDay over an explicit span, drawn whole: a
// rolling window is the span the reader asked for, so trimming its quiet ends
// would answer a different question.
func layoutFocusRange(
	all []db.ActivityRecordRow,
	subject focus,
	devices []db.DeviceRow,
	start, end time.Time,
	marks []HourMark,
) Chart {
	chart := layoutFocusRangeWith(all, subject, devices, start, end, marks, false)
	chart.Compact = true
	return chart
}

func layoutFocusRangeWith(
	all []db.ActivityRecordRow,
	subject focus,
	devices []db.DeviceRow,
	start, end time.Time,
	marks []HourMark,
	showDates bool,
) Chart {
	return layoutFocused(
		mergeAdjacentActivity(all),
		focus{records: mergeAdjacentActivity(subject.records), fill: subject.fill},
		devices,
		start, end,
		marks,
		showDates,
		nil,
	)
}

// layoutFocused draws two record sets onto one axis: context first, muted,
// then focus over it. A dashboard chart passes no context and every bar is
// focused, which is the same chart as before this split.
//
// Lanes come from the union of both sets so the two layers stay in the same
// rows: a device that only appears in the context still gets its lane, or the
// focused bars would shift up into it.
func layoutFocused(
	context []db.ActivityRecordRow,
	subject focus,
	devices []db.DeviceRow,
	start, end time.Time,
	marks []HourMark,
	showDates bool,
	scale *activeHourScale,
) Chart {
	span := end.Sub(start).Minutes()
	if scale != nil {
		span = float64(scale.kept) * 60
	}

	chart := Chart{
		WindowStart: start,
		WindowEnd:   end,
		SpanMin:     span,
		HourMarks:   marks,
	}

	names := make(map[int64]string, len(devices))
	order := make([]int64, 0, len(devices))
	for _, d := range devices {
		names[d.ID] = d.Name
		order = append(order, d.ID)
	}

	byDevice := make(map[int64][]Bar)
	collect := func(records []db.ActivityRecordRow, muted bool, fill string) {
		for i := range records {
			rec := &records[i]
			bar, ok := toBar(rec, start, end, span, i, showDates, scale)
			if !ok {
				continue
			}
			bar.Muted = muted
			if fill != "" {
				bar.Fill = fill
			}
			if _, known := names[rec.DeviceID]; !known {
				names[rec.DeviceID] = fmt.Sprintf("device %d", rec.DeviceID)
				order = append(order, rec.DeviceID)
			}
			byDevice[rec.DeviceID] = append(byDevice[rec.DeviceID], bar)
		}
	}
	collect(context, true, "")
	collect(subject.records, false, subject.fill)

	for _, id := range order {
		bars, ok := byDevice[id]
		if !ok {
			continue
		}
		chart.Lanes = append(chart.Lanes, Lane{
			DeviceID:   id,
			DeviceName: names[id],
			Bars:       bars,
		})
	}
	return chart
}

// toBar clamps one record to the visible window and positions it. It reports
// false for records that fall outside the window entirely.
func toBar(
	rec *db.ActivityRecordRow,
	start, end time.Time,
	span float64,
	index int,
	showDates bool,
	scale *activeHourScale,
) (Bar, bool) {
	from := rec.StartedAt
	if from.Before(start) {
		from = start
	}
	to := rec.EndedAt
	if to.After(end) {
		to = end
	}
	if !to.After(from) {
		return Bar{}, false
	}

	x := from.Sub(start).Minutes()
	width := to.Sub(from).Minutes()
	if scale != nil {
		// Every hour a record touches survives the compression, so its
		// endpoints stay in one run and the width is still its duration.
		x = scale.minutesAt(from, start)
		width = scale.minutesAt(to, start) - x
	}
	if minWidth := span * minBarFraction; width < minWidth {
		width = minWidth
	}
	if x+width > span {
		width = span - x
	}
	if width <= 0 {
		return Bar{}, false
	}

	timeFormat := "15:04"
	if showDates {
		timeFormat = "Mon 2 15:04"
	}

	return Bar{
		X:       x,
		Width:   width,
		Fill:    appColor(rec.AppName),
		AppName: rec.AppName,
		Title:   rec.Title,
		TimeRange: fmt.Sprintf(
			"%s - %s",
			rec.StartedAt.In(start.Location()).Format(timeFormat),
			rec.EndedAt.In(start.Location()).Format(timeFormat),
		),
		Bucket: index % 12,
	}, true
}

// hourMarks walks the day an hour at a time rather than counting to 24,
// so a 23- or 25-hour DST day gets the right number of gridlines.
func hourMarks(start, end time.Time) []HourMark {
	var marks []HourMark
	for t := start; t.Before(end); t = t.Add(time.Hour) {
		marks = append(marks, HourMark{Label: t.Format("15"), SpanHours: 1})
	}
	return marks
}

// tickMarks divides a rolling window into equal cells of step.
//
// Unlike hourMarks it labels the minute too: a window ending now starts at
// 14:20, and an axis of bare hours would put "15" over a cell that begins at
// 14:20 and mean the wrong thing by forty minutes.
func tickMarks(start, end time.Time, step time.Duration) []HourMark {
	var marks []HourMark
	for t := start; t.Before(end); t = t.Add(step) {
		marks = append(marks, HourMark{Label: t.Format("15:04"), SpanHours: 1})
	}
	return marks
}

func dayMarks(start, end time.Time) []HourMark {
	var marks []HourMark
	for day := start; day.Before(end); {
		next := day.AddDate(0, 0, 1)
		marks = append(marks, HourMark{
			Label:     day.Format("Mon 2"),
			SpanHours: int(next.Sub(day) / time.Hour),
		})
		day = next
	}
	return marks
}

// appColor maps an app name to a stable hue, so the same app keeps its
// colour across days and devices.
func appColor(app string) string {
	fill, _, _ := appPalette(app)
	return fill
}

// appPalette returns the timeline fill, the monogram chip's background, and
// the text colour that reads on that chip.
//
// The chip is deliberately paler than the bar. A monogram stands in for a
// real icon, and at full saturation the placeholders shouted over the genuine
// artwork beside them. Muting also buys contrast headroom: the worst hue on
// the bar palette clears 4.5:1 by two percent, while the chip palette clears
// it by more than half.
func appPalette(app string) (string, string, string) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(app))
	hue := int(h.Sum32() % 360)
	fill := fmt.Sprintf("hsl(%d 62%% 48%%)", hue)
	chip := fmt.Sprintf("hsl(%d %d%% %d%%)", hue, monogramSaturation, monogramLightness)
	return fill, chip, monogramForeground(hue)
}

func monogramForeground(hue int) string {
	if hslRelativeLuminance(hue, monogramSaturation/100.0, monogramLightness/100.0) > 0.179 {
		return monogramDark
	}
	return monogramLight
}

func hslRelativeLuminance(hue int, saturation, lightness float64) float64 {
	h := float64(hue) / 360
	q := lightness * (1 + saturation)
	if lightness >= 0.5 {
		q = lightness + saturation - lightness*saturation
	}
	p := 2*lightness - q
	r := hslChannel(p, q, h+1.0/3.0)
	g := hslChannel(p, q, h)
	b := hslChannel(p, q, h-1.0/3.0)
	return 0.2126*linearRGB(r) + 0.7152*linearRGB(g) + 0.0722*linearRGB(b)
}

func hslChannel(p, q, t float64) float64 {
	if t < 0 {
		t++
	}
	if t > 1 {
		t--
	}
	switch {
	case t < 1.0/6.0:
		return p + (q-p)*6*t
	case t < 0.5:
		return q
	case t < 2.0/3.0:
		return p + (q-p)*(2.0/3.0-t)*6
	default:
		return p
	}
}

func linearRGB(channel float64) float64 {
	if channel <= 0.04045 {
		return channel / 12.92
	}
	return math.Pow((channel+0.055)/1.055, 2.4)
}
