package server

import (
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/db"
)

func scaleDay() time.Time {
	return time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
}

// Every case here uses one device; lanes are covered elsewhere.
const scaleDeviceID = 7

func scaleRecord(app string, fromHour, fromMin, toHour, toMin int) db.ActivityRecordRow {
	day := scaleDay()
	return db.ActivityRecordRow{
		DeviceID:  scaleDeviceID,
		AppName:   app,
		StartedAt: day.Add(time.Duration(fromHour)*time.Hour + time.Duration(fromMin)*time.Minute),
		EndedAt:   day.Add(time.Duration(toHour)*time.Hour + time.Duration(toMin)*time.Minute),
	}
}

func TestLayoutDropsHoursWithoutActivity(t *testing.T) {
	t.Parallel()

	records := []db.ActivityRecordRow{
		scaleRecord(testFirefoxApp, 9, 0, 9, 30),
		scaleRecord(testGhosttyApp, 17, 15, 17, 45),
	}
	chart := layout(records, []db.DeviceRow{{ID: scaleDeviceID, Name: testLaptop}}, scaleDay())

	if len(chart.HourMarks) != 2 {
		t.Fatalf("axis has %d cells, want the two worked hours", len(chart.HourMarks))
	}
	if chart.HourMarks[0].Label != "09" || chart.HourMarks[1].Label != "17" {
		t.Errorf("labels = %q, %q; want 09 and 17", chart.HourMarks[0].Label, chart.HourMarks[1].Label)
	}
	if chart.SpanMin != 120 {
		t.Errorf("span = %v minutes, want 120 for two kept hours", chart.SpanMin)
	}

	// The seam has to be visible: these two hours are eight apart.
	if chart.HourMarks[0].GapBefore {
		t.Error("the first cell cannot follow a gap")
	}
	if !chart.HourMarks[1].GapBefore {
		t.Error("17:00 follows dropped hours and is not marked as a break")
	}
}

// Compression removes empty time, never rescales the bars that remain.
func TestLayoutCompressionPreservesBarWidths(t *testing.T) {
	t.Parallel()

	records := []db.ActivityRecordRow{
		scaleRecord(testFirefoxApp, 9, 0, 9, 30),
		scaleRecord(testGhosttyApp, 17, 15, 17, 45),
	}
	chart := layout(records, []db.DeviceRow{{ID: scaleDeviceID, Name: testLaptop}}, scaleDay())
	bars := chart.Lanes[0].Bars

	if len(bars) != 2 {
		t.Fatalf("got %d bars, want 2", len(bars))
	}
	for i, want := range []float64{30, 30} {
		if bars[i].Width != want {
			t.Errorf("bar %d width = %v, want %v minutes", i, bars[i].Width, want)
		}
	}
	// First kept hour starts the axis; the second begins 60 units in, and
	// the record is a quarter hour into it.
	if bars[0].X != 0 {
		t.Errorf("first bar x = %v, want 0", bars[0].X)
	}
	if bars[1].X != 75 {
		t.Errorf("second bar x = %v, want 75", bars[1].X)
	}
}

// A record spanning several hours keeps them all, so it stays contiguous.
func TestLayoutKeepsEveryHourARecordTouches(t *testing.T) {
	t.Parallel()

	records := []db.ActivityRecordRow{
		scaleRecord(testFirefoxApp, 9, 30, 12, 30),
		scaleRecord(testGhosttyApp, 20, 0, 20, 10),
	}
	chart := layout(records, []db.DeviceRow{{ID: scaleDeviceID, Name: testLaptop}}, scaleDay())

	// 09, 10, 11, 12 for the long record, then 20.
	if len(chart.HourMarks) != 5 {
		t.Fatalf("axis has %d cells, want 5", len(chart.HourMarks))
	}
	if width := chart.Lanes[0].Bars[0].Width; width != 180 {
		t.Errorf("three-hour record width = %v, want 180 minutes", width)
	}
}

// An interval ending exactly on the hour does not reach into the next one.
func TestLayoutDoesNotKeepAnHourTouchedOnlyAtItsEdge(t *testing.T) {
	t.Parallel()

	records := []db.ActivityRecordRow{scaleRecord(testFirefoxApp, 9, 0, 10, 0)}
	chart := layout(records, []db.DeviceRow{{ID: scaleDeviceID, Name: testLaptop}}, scaleDay())

	if len(chart.HourMarks) != 1 || chart.HourMarks[0].Label != "09" {
		t.Errorf("axis = %+v, want only the 09 hour", chart.HourMarks)
	}
}

// A day whose activity already fills the window keeps the plain linear axis.
func TestLayoutSkipsCompressionWhenNothingIsEmpty(t *testing.T) {
	t.Parallel()

	records := make([]db.ActivityRecordRow, 0, 24)
	for hour := range 24 {
		records = append(records, scaleRecord(testFirefoxApp, hour, 0, hour, 59))
	}
	chart := layout(records, []db.DeviceRow{{ID: scaleDeviceID, Name: testLaptop}}, scaleDay())

	if len(chart.HourMarks) != 24 {
		t.Fatalf("axis has %d cells, want all 24", len(chart.HourMarks))
	}
	for i, mark := range chart.HourMarks {
		if mark.GapBefore {
			t.Errorf("cell %d (%s) marked as a break in a full day", i, mark.Label)
		}
	}
}

func TestLayoutHandlesNoRecords(t *testing.T) {
	t.Parallel()

	chart := layout(nil, []db.DeviceRow{{ID: scaleDeviceID, Name: testLaptop}}, scaleDay())
	if len(chart.Lanes) != 0 {
		t.Errorf("lanes = %d, want none for an empty day", len(chart.Lanes))
	}
	if chart.SpanMin <= 0 {
		t.Errorf("span = %v, want a positive width even when empty", chart.SpanMin)
	}
}
