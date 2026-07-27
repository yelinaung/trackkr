package server

import (
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/db"
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
		{"spring forward", time.Date(2026, 3, 8, 9, 30, 0, 0, loc), 23},
		{"fall back", time.Date(2026, 11, 1, 9, 30, 0, 0, loc), 25},
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

func TestHourMarksFollowDSTSpan(t *testing.T) {
	t.Parallel()
	loc := newYork(t)

	tests := []struct {
		name string
		day  time.Time
		want int
	}{
		{"ordinary day", time.Date(2026, 1, 15, 0, 0, 0, 0, loc), 24},
		{"spring forward", time.Date(2026, 3, 8, 0, 0, 0, 0, loc), 23},
		{"fall back", time.Date(2026, 11, 1, 0, 0, 0, 0, loc), 25},
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

	if bar.X != 1430 {
		t.Errorf("X = %v, want 1430 (23:50)", bar.X)
	}
	if bar.Width != 10 {
		t.Errorf("Width = %v, want 10 minutes before midnight", bar.Width)
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

	bar := layout(recs, nil, day).Lanes[0].Bars[0]
	if bar.Width < minBarMinutes {
		t.Errorf("Width = %v, want at least %v", bar.Width, minBarMinutes)
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
