package server

import (
	"fmt"
	"hash/fnv"
	"time"

	"github.com/yelinaung/trackkr/internal/db"
)

// minBarFraction floors a bar's width as a fraction of the day rather
// than as a fixed number of minutes.
//
// A one-minute floor is meaningless on screen: the chart is 1440 units
// wide, so at its minimum rendered width a minute is under half a
// pixel and a real visit is invisible. This is a visibility affordance,
// not a claim about duration -- the tooltip carries the true range, and
// the totals below the chart are computed from the data, not the bars.
const minBarFraction = 1.0 / 480.0

// Bar is one activity block, positioned in minutes from the start of the
// day. Geometry travels to the template as SVG presentation attributes,
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
type Chart struct {
	DayStart  time.Time
	DayEnd    time.Time
	SpanMin   float64
	HourMarks []HourMark
	Lanes     []Lane
}

// HourMark labels one hour cell of the axis. The axis is HTML rather
// than SVG text, which would be distorted by the non-uniform scaling the
// bars need, so equal-width cells carry the position and only the label
// travels.
type HourMark struct {
	Label string
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

// layout converts records into positioned bars for one day. Records are
// clamped to the day, which is what the overlapping database query makes
// possible for spans crossing midnight.
func layout(records []db.ActivityRecordRow, devices []db.DeviceRow, day time.Time) Chart {
	start, end := dayBounds(day)
	span := end.Sub(start).Minutes()

	chart := Chart{
		DayStart:  start,
		DayEnd:    end,
		SpanMin:   span,
		HourMarks: hourMarks(start, end),
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
		bar, ok := toBar(rec, start, end, span, i)
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

// toBar clamps one record to the day window and positions it. It reports
// false for records that fall outside the window entirely.
func toBar(rec *db.ActivityRecordRow, start, end time.Time, span float64, index int) (Bar, bool) {
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

	return Bar{
		X:         x,
		Width:     width,
		Fill:      appColor(rec.AppName),
		AppName:   rec.AppName,
		Title:     rec.Title,
		TimeRange: fmt.Sprintf("%s - %s", rec.StartedAt.In(start.Location()).Format("15:04"), rec.EndedAt.In(start.Location()).Format("15:04")),
		Bucket:    index % 12,
	}, true
}

// hourMarks walks the day an hour at a time rather than counting to 24,
// so a 23- or 25-hour DST day gets the right number of gridlines.
func hourMarks(start, end time.Time) []HourMark {
	var marks []HourMark
	for t := start; t.Before(end); t = t.Add(time.Hour) {
		marks = append(marks, HourMark{Label: t.Format("15")})
	}
	return marks
}

// appColor maps an app name to a stable hue, so the same app keeps its
// colour across days and devices.
func appColor(app string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(app))
	hue := int(h.Sum32() % 360)
	return fmt.Sprintf("hsl(%d 62%% 48%%)", hue)
}
