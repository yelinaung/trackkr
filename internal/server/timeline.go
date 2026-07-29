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
}

// HourMark labels one time cell of the axis. The axis is HTML rather
// than SVG text, which would be distorted by the non-uniform scaling the
// bars need. SpanHours keeps DST-short or DST-long days aligned in a week.
type HourMark struct {
	Label     string
	SpanHours int
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

// layout converts records into positioned bars for one day. Records are
// clamped to the drawn window, which is what the overlapping database
// query makes possible for spans crossing midnight.
func layout(records []db.ActivityRecordRow, devices []db.DeviceRow, day time.Time) Chart {
	dayStart, dayEnd := dayBounds(day)
	start, end := chartWindow(records, dayStart, dayEnd)
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
	span := end.Sub(start).Minutes()

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
	for i := range records {
		rec := &records[i]
		bar, ok := toBar(rec, start, end, span, i, showDates)
		if !ok {
			continue
		}
		if _, known := names[rec.DeviceID]; !known {
			names[rec.DeviceID] = fmt.Sprintf("device %d", rec.DeviceID)
			order = append(order, rec.DeviceID)
		}
		byDevice[rec.DeviceID] = append(byDevice[rec.DeviceID], bar)
	}

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
	fill, _ := appPalette(app)
	return fill
}

func appPalette(app string) (string, string) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(app))
	hue := int(h.Sum32() % 360)
	fill := fmt.Sprintf("hsl(%d 62%% 48%%)", hue)
	return fill, monogramForeground(hue)
}

func monogramForeground(hue int) string {
	if hslRelativeLuminance(hue, 0.62, 0.48) > 0.179 {
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
