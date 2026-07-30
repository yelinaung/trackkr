package server

import (
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/db"
)

// The two DST days name themselves often enough that the linter asks
// for constants.
const (
	testSpringForward = "spring forward"
	testFallBack      = "fall back"
)

// newYork observes DST, so it exercises the 23- and 25-hour days that a
// fixed 24-hour span would render wrong.
func newYork(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	return loc
}

func TestDayBoundsSpans(t *testing.T) {
	t.Parallel()
	loc := newYork(t)

	tests := []struct {
		name    string
		day     time.Time
		wantHrs float64
	}{
		{"ordinary day", time.Date(2026, 1, 15, 9, 30, 0, 0, loc), 24},
		{testSpringForward, time.Date(2026, 3, 8, 9, 30, 0, 0, loc), 23},
		{testFallBack, time.Date(2026, 11, 1, 9, 30, 0, 0, loc), 25},
		{"utc control", time.Date(2026, 3, 8, 9, 30, 0, 0, time.UTC), 24},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			start, end := dayBounds(tt.day)

			if got := end.Sub(start).Hours(); got != tt.wantHrs {
				t.Errorf("span = %vh, want %vh", got, tt.wantHrs)
			}
			if start.Hour() != 0 || start.Minute() != 0 {
				t.Errorf("start = %v, want midnight", start)
			}
			if end.Hour() != 0 || end.Minute() != 0 {
				t.Errorf("end = %v, want midnight", end)
			}
		})
	}
}

func TestWeekBoundsUseMondayAndFollowDST(t *testing.T) {
	t.Parallel()
	loc := newYork(t)

	tests := []struct {
		name      string
		day       time.Time
		wantStart time.Time
		wantHours float64
	}{
		{
			"midweek",
			time.Date(2026, 5, 6, 14, 30, 0, 0, loc),
			time.Date(2026, 5, 4, 0, 0, 0, 0, loc),
			168,
		},
		{
			"sunday",
			time.Date(2026, 5, 10, 14, 30, 0, 0, loc),
			time.Date(2026, 5, 4, 0, 0, 0, 0, loc),
			168,
		},
		{
			testSpringForward,
			time.Date(2026, 3, 8, 14, 30, 0, 0, loc),
			time.Date(2026, 3, 2, 0, 0, 0, 0, loc),
			167,
		},
		{
			testFallBack,
			time.Date(2026, 11, 1, 14, 30, 0, 0, loc),
			time.Date(2026, 10, 26, 0, 0, 0, 0, loc),
			169,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			start, end := weekBounds(tt.day)
			if !start.Equal(tt.wantStart) {
				t.Errorf("start = %v, want %v", start, tt.wantStart)
			}
			if start.Weekday() != time.Monday || end.Weekday() != time.Monday {
				t.Errorf("range = %v..%v, want Monday boundaries", start, end)
			}
			if got := end.Sub(start).Hours(); got != tt.wantHours {
				t.Errorf("span = %vh, want %vh", got, tt.wantHours)
			}
		})
	}
}

func TestWeekLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		start time.Time
		want  string
	}{
		{"same month", time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC), "4-10 May 2026"},
		{"across months", time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC), "27 April - 3 May 2026"},
		{"across years", time.Date(2025, 12, 29, 0, 0, 0, 0, time.UTC), "29 December 2025 - 4 January 2026"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := weekLabel(tt.start, tt.start.AddDate(0, 0, 7)); got != tt.want {
				t.Errorf("weekLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseDashboardView(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]string{
		"":      dashboardViewDay,
		"day":   dashboardViewDay,
		"month": dashboardViewDay,
		"week":  dashboardViewWeek,
		"WEEK":  dashboardViewDay,
	} {
		if got := parseDashboardView(raw); got != want {
			t.Errorf("parseDashboardView(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestHourMarksFollowDSTSpan(t *testing.T) {
	t.Parallel()
	loc := newYork(t)

	tests := []struct {
		name string
		day  time.Time
		want int
	}{
		{"ordinary day", time.Date(2026, 1, 15, 0, 0, 0, 0, loc), 24},
		{testSpringForward, time.Date(2026, 3, 8, 0, 0, 0, 0, loc), 23},
		{testFallBack, time.Date(2026, 11, 1, 0, 0, 0, 0, loc), 25},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			start, end := dayBounds(tt.day)

			if got := len(hourMarks(start, end)); got != tt.want {
				t.Errorf("hour marks = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDayMarksFollowDSTSpan(t *testing.T) {
	t.Parallel()
	loc := newYork(t)
	start, end := weekBounds(time.Date(2026, 3, 8, 12, 0, 0, 0, loc))
	marks := dayMarks(start, end)

	if len(marks) != 7 {
		t.Fatalf("day marks = %d, want 7", len(marks))
	}
	var spanHours int
	for _, mark := range marks {
		spanHours += mark.SpanHours
	}
	if spanHours != 167 {
		t.Errorf("axis span = %dh, want 167h", spanHours)
	}
	if marks[0].Label != "Mon 2" || marks[6].Label != "Sun 8" {
		t.Errorf("labels = %q..%q, want Mon 2..Sun 8", marks[0].Label, marks[6].Label)
	}
	if marks[6].SpanHours != 23 {
		t.Errorf("spring-forward day span = %dh, want 23h", marks[6].SpanHours)
	}
}

func TestLayoutWeekDrawsFullRangeWithDatedTooltips(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 7)
	records := []db.ActivityRecordRow{
		{
			DeviceID:  7,
			AppName:   testFirefoxLower,
			StartedAt: start.Add(time.Hour),
			EndedAt:   start.Add(2 * time.Hour),
		},
		{
			DeviceID:  7,
			AppName:   testAppCode,
			StartedAt: start.Add(6*24*time.Hour + time.Hour),
			EndedAt:   start.Add(6*24*time.Hour + 2*time.Hour),
		},
	}
	devices := []db.DeviceRow{{ID: 7, Name: testLaptop}}

	chart := layoutWeek(records, devices, start, end)
	if !chart.WindowStart.Equal(start) || !chart.WindowEnd.Equal(end) {
		t.Errorf("window = %v..%v, want %v..%v", chart.WindowStart, chart.WindowEnd, start, end)
	}
	if chart.SpanMin != 7*24*60 {
		t.Errorf("span = %v minutes, want %d", chart.SpanMin, 7*24*60)
	}
	if len(chart.HourMarks) != 7 {
		t.Fatalf("marks = %d, want 7", len(chart.HourMarks))
	}
	if len(chart.Lanes) != 1 || len(chart.Lanes[0].Bars) != 2 {
		t.Fatalf("lanes = %+v, want one lane with two bars", chart.Lanes)
	}
	if got := chart.Lanes[0].Bars[0].TimeRange; got != "Mon 4 01:00 - Mon 4 02:00" {
		t.Errorf("first tooltip = %q", got)
	}
	if got := chart.Lanes[0].Bars[1].TimeRange; got != "Sun 10 01:00 - Sun 10 02:00" {
		t.Errorf("last tooltip = %q", got)
	}
}

func TestLayoutClampsCrossMidnightRecord(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	day := time.Date(2026, 5, 4, 0, 0, 0, 0, loc)

	// 23:50 the previous evening until 00:20 on the rendered day.
	rec := db.ActivityRecordRow{
		DeviceID:  1,
		AppName:   testFirefoxLower,
		StartedAt: time.Date(2026, 5, 3, 23, 50, 0, 0, loc),
		EndedAt:   time.Date(2026, 5, 4, 0, 20, 0, 0, loc),
	}

	chart := layout([]db.ActivityRecordRow{rec}, nil, day)
	if len(chart.Lanes) != 1 || len(chart.Lanes[0].Bars) != 1 {
		t.Fatalf("expected one bar, got %+v", chart.Lanes)
	}

	bar := chart.Lanes[0].Bars[0]
	if bar.X != 0 {
		t.Errorf("X = %v, want 0 (clamped to midnight)", bar.X)
	}
	if bar.Width != 20 {
		t.Errorf("Width = %v, want 20 minutes inside the day", bar.Width)
	}
}

func TestLayoutClampsRecordRunningPastMidnight(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	day := time.Date(2026, 5, 3, 0, 0, 0, 0, loc)

	rec := db.ActivityRecordRow{
		DeviceID:  1,
		AppName:   testFirefoxLower,
		StartedAt: time.Date(2026, 5, 3, 23, 50, 0, 0, loc),
		EndedAt:   time.Date(2026, 5, 4, 0, 20, 0, 0, loc),
	}

	chart := layout([]db.ActivityRecordRow{rec}, nil, day)
	bar := chart.Lanes[0].Bars[0]

	// Positions are relative to the drawn window, which ends at
	// midnight here, so the bar sits ten minutes from its right edge.
	if want := chart.SpanMin - 10; bar.X != want {
		t.Errorf("X = %v, want %v (23:50 within the window)", bar.X, want)
	}
	if bar.Width != 10 {
		t.Errorf("Width = %v, want 10 minutes before midnight", bar.Width)
	}
	if !chart.WindowEnd.Equal(time.Date(2026, 5, 4, 0, 0, 0, 0, loc)) {
		t.Errorf("WindowEnd = %v, want midnight", chart.WindowEnd)
	}
	if bar.X+bar.Width > chart.SpanMin {
		t.Errorf("bar runs past the chart: %v > %v", bar.X+bar.Width, chart.SpanMin)
	}
}

func TestLayoutSkipsRecordsOutsideDay(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	day := time.Date(2026, 5, 4, 0, 0, 0, 0, loc)

	recs := []db.ActivityRecordRow{{
		DeviceID:  1,
		AppName:   testFirefoxLower,
		StartedAt: time.Date(2026, 5, 2, 10, 0, 0, 0, loc),
		EndedAt:   time.Date(2026, 5, 2, 11, 0, 0, 0, loc),
	}}

	if chart := layout(recs, nil, day); len(chart.Lanes) != 0 {
		t.Errorf("expected no lanes, got %+v", chart.Lanes)
	}
}

func TestLayoutGivesShortRecordsAMinimumWidth(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	day := time.Date(2026, 5, 4, 0, 0, 0, 0, loc)

	recs := []db.ActivityRecordRow{{
		DeviceID:  1,
		AppName:   "slack",
		StartedAt: time.Date(2026, 5, 4, 10, 0, 0, 0, loc),
		EndedAt:   time.Date(2026, 5, 4, 10, 0, 5, 0, loc),
	}}

	chart := layout(recs, nil, day)
	bar := chart.Lanes[0].Bars[0]

	// The floor scales with the day so a bar is never sub-pixel: a
	// fixed one-minute floor renders at under half a pixel.
	want := chart.SpanMin * minBarFraction
	if bar.Width < want {
		t.Errorf("Width = %v, want at least %v", bar.Width, want)
	}
	if bar.Width > 5 {
		t.Errorf("Width = %v, want the floor to stay small enough to read as brief", bar.Width)
	}
}

func TestLayoutGroupsLanesByDevice(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	day := time.Date(2026, 5, 4, 0, 0, 0, 0, loc)

	recs := []db.ActivityRecordRow{
		{DeviceID: 1, AppName: testFirefoxLower, StartedAt: day.Add(time.Hour), EndedAt: day.Add(2 * time.Hour)},
		{DeviceID: 2, AppName: testAppCode, StartedAt: day.Add(time.Hour), EndedAt: day.Add(3 * time.Hour)},
		{DeviceID: 1, AppName: testAppCode, StartedAt: day.Add(4 * time.Hour), EndedAt: day.Add(5 * time.Hour)},
	}
	devices := []db.DeviceRow{{ID: 1, Name: testLaptop}, {ID: 2, Name: testDesktop}}

	chart := layout(recs, devices, day)
	if len(chart.Lanes) != 2 {
		t.Fatalf("lanes = %d, want 2", len(chart.Lanes))
	}
	if chart.Lanes[0].DeviceName != testLaptop || len(chart.Lanes[0].Bars) != 2 {
		t.Errorf("lane 0 = %+v, want laptop with 2 bars", chart.Lanes[0])
	}
	if chart.Lanes[1].DeviceName != testDesktop || len(chart.Lanes[1].Bars) != 1 {
		t.Errorf("lane 1 = %+v, want desktop with 1 bar", chart.Lanes[1])
	}
}

func TestAppColorIsStableAndDistinct(t *testing.T) {
	t.Parallel()

	firefox, again, code := appColor(testFirefoxLower), appColor(testFirefoxLower), appColor(testAppCode)

	if firefox != again {
		t.Error("same app produced different colours")
	}
	if firefox == code {
		t.Error("different apps produced the same colour")
	}
}

func TestMonogramForegroundMeetsContrastTarget(t *testing.T) {
	t.Parallel()

	for hue := range 360 {
		// The chip the monogram actually sits on, not the timeline bar.
		background := hslRelativeLuminance(
			hue,
			monogramSaturation/100.0,
			monogramLightness/100.0,
		)
		foreground := 1.0
		if monogramForeground(hue) == monogramDark {
			foreground = 0
		}
		lighter := max(background, foreground)
		darker := min(background, foreground)
		contrast := (lighter + 0.05) / (darker + 0.05)
		if contrast < 4.5 {
			t.Errorf("hue %d contrast = %.2f, want at least 4.5", hue, contrast)
		}
	}
}

func TestBarBucketsCycleForStagger(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	day := time.Date(2026, 5, 4, 0, 0, 0, 0, loc)

	recs := make([]db.ActivityRecordRow, 0, 14)
	for i := range 14 {
		recs = append(recs, db.ActivityRecordRow{
			DeviceID:  1,
			AppName:   testFirefoxLower,
			StartedAt: day.Add(time.Duration(i) * time.Hour),
			EndedAt:   day.Add(time.Duration(i)*time.Hour + 30*time.Minute),
		})
	}

	bars := layout(recs, nil, day).Lanes[0].Bars
	if bars[0].Bucket != 0 || bars[11].Bucket != 11 || bars[12].Bucket != 0 {
		t.Errorf("buckets did not cycle: %d %d %d", bars[0].Bucket, bars[11].Bucket, bars[12].Bucket)
	}
}

// rec is a one-device record spanning wall-clock times on 4 May 2026,
// the ordinary 24-hour day every window case is measured against.
func rec(loc *time.Location, fromH, fromM, toH, toM int) db.ActivityRecordRow {
	at := func(h, m int) time.Time {
		return time.Date(2026, 5, 4, h, m, 0, 0, loc)
	}
	return db.ActivityRecordRow{
		DeviceID:  1,
		AppName:   testFirefoxLower,
		StartedAt: at(fromH, fromM),
		EndedAt:   at(toH, toM),
	}
}

func TestChartWindowTrimsEmptyHours(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	day := time.Date(2026, 5, 4, 0, 0, 0, 0, loc)
	dayStart, dayEnd := dayBounds(day)

	tests := []struct {
		name               string
		records            []db.ActivityRecordRow
		wantStart, wantEnd int
	}{
		{
			// 09:12-17:40 floors to 09:00, ceils to 18:00, then a
			// padding hour each way.
			"a working day",
			[]db.ActivityRecordRow{rec(loc, 9, 12, 17, 40)},
			8, 19,
		},
		{
			// Three hours of span grow to the six-hour floor, half
			// each way with the odd hour going forward.
			"a sparse morning",
			[]db.ActivityRecordRow{rec(loc, 9, 5, 9, 25)},
			6, 12,
		},
		{
			// Nothing to trim: padding and the floor both run past
			// the ends of the day.
			"dawn to dusk",
			[]db.ActivityRecordRow{rec(loc, 0, 5, 23, 55)},
			0, 24,
		},
		{
			// The clamp at midnight is spent on the other side, so
			// the window keeps its full width.
			"first thing in the morning",
			[]db.ActivityRecordRow{rec(loc, 0, 10, 0, 30)},
			0, 6,
		},
		{
			"last thing at night",
			[]db.ActivityRecordRow{rec(loc, 23, 10, 23, 30)},
			18, 24,
		},
		{
			// Two devices, or two distant sessions: the window is the
			// union, never one record's neighbourhood.
			"a gap in the middle stays drawn",
			[]db.ActivityRecordRow{
				rec(loc, 9, 0, 9, 30),
				rec(loc, 16, 0, 16, 30),
			},
			8, 18,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			start, end := chartWindow(tt.records, dayStart, dayEnd)

			if got := int(start.Sub(dayStart).Hours()); got != tt.wantStart {
				t.Errorf("start = %02d:00, want %02d:00", got, tt.wantStart)
			}
			if got := int(end.Sub(dayStart).Hours()); got != tt.wantEnd {
				t.Errorf("end = %02d:00, want %02d:00", got, tt.wantEnd)
			}
			if start.Minute() != 0 || end.Minute() != 0 {
				t.Errorf("window = %v..%v, want whole hours so the axis lands on the clock", start, end)
			}
			if end.Sub(start) < minChartWindow {
				t.Errorf("window = %v, want at least %v", end.Sub(start), minChartWindow)
			}
		})
	}
}

func TestChartWindowFallsBackToTheWholeDay(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	dayStart, dayEnd := dayBounds(time.Date(2026, 5, 4, 0, 0, 0, 0, loc))

	// A record entirely outside the day contributes no extent, and an
	// extent-free day must not collapse to a zero-width chart.
	outside := db.ActivityRecordRow{
		DeviceID:  1,
		AppName:   testFirefoxLower,
		StartedAt: time.Date(2026, 5, 6, 9, 0, 0, 0, loc),
		EndedAt:   time.Date(2026, 5, 6, 10, 0, 0, 0, loc),
	}

	for name, records := range map[string][]db.ActivityRecordRow{
		"no records":       nil,
		"nothing overlaps": {outside},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			start, end := chartWindow(records, dayStart, dayEnd)
			if !start.Equal(dayStart) || !end.Equal(dayEnd) {
				t.Errorf("window = %v..%v, want the whole day", start, end)
			}
		})
	}
}

// A window that keeps its own hour grid still has to survive a day that
// is not 24 hours long.
func TestChartWindowHandlesDSTDays(t *testing.T) {
	t.Parallel()
	loc := newYork(t)

	tests := []struct {
		name string
		day  time.Time
	}{
		{testSpringForward, time.Date(2026, 3, 8, 0, 0, 0, 0, loc)},
		{testFallBack, time.Date(2026, 11, 1, 0, 0, 0, 0, loc)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dayStart, dayEnd := dayBounds(tt.day)
			records := []db.ActivityRecordRow{{
				DeviceID:  1,
				AppName:   testFirefoxLower,
				StartedAt: dayStart.Add(9 * time.Hour),
				EndedAt:   dayStart.Add(10 * time.Hour),
			}}

			start, end := chartWindow(records, dayStart, dayEnd)
			if start.Before(dayStart) || end.After(dayEnd) {
				t.Errorf("window %v..%v escapes the day %v..%v", start, end, dayStart, dayEnd)
			}
			if start.Minute() != 0 || end.Minute() != 0 {
				t.Errorf("window = %v..%v, want whole hours", start, end)
			}
			// One cell per hour of the window, or the labels drift
			// away from the bars they name.
			if got, want := len(hourMarks(start, end)), int(end.Sub(start).Hours()); got != want {
				t.Errorf("hour marks = %d, want %d", got, want)
			}
		})
	}
}
