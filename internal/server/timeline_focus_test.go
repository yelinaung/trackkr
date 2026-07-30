package server

import (
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/db"
)

// testFocusFill is the subject colour the detail page would pass in.
const testFocusFill = "hsl(120 62% 48%)"

func countBars(chart *Chart) (muted, focused int) {
	for _, lane := range chart.Lanes {
		for _, bar := range lane.Bars {
			if bar.Muted {
				muted++
				continue
			}
			focused++
		}
	}
	return muted, focused
}

// A focused chart draws the whole period, not just the subject: the context
// is what makes "two hours" legible as a morning rather than a number.
func TestLayoutFocusDayKeepsContextMuted(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	devices := []db.DeviceRow{{ID: 7, Name: testLaptop}}
	all := []db.ActivityRecordRow{
		{
			DeviceID: 7, AppName: testAppCode,
			StartedAt: day.Add(9 * time.Hour), EndedAt: day.Add(10 * time.Hour),
		},
		{
			DeviceID: 7, AppName: testFirefoxLower,
			StartedAt: day.Add(10 * time.Hour), EndedAt: day.Add(11 * time.Hour),
		},
	}

	chart := layoutFocusDay(all, focus{records: all[:1], fill: testFocusFill}, devices, day)

	muted, focused := countBars(&chart)
	if muted != 2 {
		t.Errorf("muted bars = %d, want the whole day drawn as context", muted)
	}
	if focused != 1 {
		t.Errorf("focused bars = %d, want only the subject", focused)
	}
	if len(chart.Lanes) != 1 {
		t.Fatalf("lanes = %d, want one per device", len(chart.Lanes))
	}
	// Context is drawn first so the subject paints over it.
	if chart.Lanes[0].Bars[0].Muted == false {
		t.Error("focused bars are painted under the context")
	}
}

// A site is observed through a browser, so colouring its bars by application
// name would paint them in the browser's hue while the legend beside the
// chart showed the site's.
func TestLayoutFocusPaintsTheSubjectsColour(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	devices := []db.DeviceRow{{ID: 7, Name: testLaptop}}
	all := []db.ActivityRecordRow{{
		DeviceID: 7, AppName: db.ChromeAppName,
		StartedAt: day.Add(9 * time.Hour), EndedAt: day.Add(10 * time.Hour),
	}}

	chart := layoutFocusDay(all, focus{records: all, fill: testFocusFill}, devices, day)

	for _, bar := range chart.Lanes[0].Bars {
		if bar.Muted {
			if bar.Fill == testFocusFill {
				t.Error("context bars took the subject's colour")
			}
			continue
		}
		if bar.Fill != testFocusFill {
			t.Errorf("focused bar fill = %q, want the subject's %q", bar.Fill, testFocusFill)
		}
	}
}

// A device the subject was never used on still gets its lane, or the focused
// bars slide up into a row that belongs to another machine.
func TestLayoutFocusKeepsEveryDeviceLane(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	devices := []db.DeviceRow{{ID: 7, Name: testLaptop}, {ID: 8, Name: testDesktop}}
	all := []db.ActivityRecordRow{
		{
			DeviceID: 7, AppName: testAppCode,
			StartedAt: day.Add(9 * time.Hour), EndedAt: day.Add(10 * time.Hour),
		},
		{
			DeviceID: 8, AppName: testFirefoxLower,
			StartedAt: day.Add(9 * time.Hour), EndedAt: day.Add(10 * time.Hour),
		},
	}

	chart := layoutFocusDay(all, focus{records: all[:1], fill: testFocusFill}, devices, day)

	if len(chart.Lanes) != 2 {
		t.Fatalf("lanes = %d, want both devices", len(chart.Lanes))
	}
	if chart.Lanes[0].DeviceID != 7 || chart.Lanes[1].DeviceID != 8 {
		t.Errorf("lane order = %d, %d, want the device order", chart.Lanes[0].DeviceID, chart.Lanes[1].DeviceID)
	}
}

// The dashboard chart is the focused chart with nothing to contrast against,
// so nothing on it may come out muted.
func TestLayoutLeavesEveryBarFocused(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	devices := []db.DeviceRow{{ID: 7, Name: testLaptop}}
	records := []db.ActivityRecordRow{{
		DeviceID: 7, AppName: testAppCode,
		StartedAt: day.Add(9 * time.Hour), EndedAt: day.Add(10 * time.Hour),
	}}

	chart := layout(records, devices, day)

	muted, focused := countBars(&chart)
	if muted != 0 || focused != 1 {
		t.Errorf("dashboard chart = %d muted, %d focused; want 0 and 1", muted, focused)
	}
}

// The week view compresses nothing, so both layers share the day axis.
func TestLayoutFocusWeekSharesTheAxis(t *testing.T) {
	t.Parallel()
	weekStart := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	weekEnd := weekStart.AddDate(0, 0, 7)
	devices := []db.DeviceRow{{ID: 7, Name: testLaptop}}
	all := []db.ActivityRecordRow{
		{
			DeviceID: 7, AppName: testAppCode,
			StartedAt: weekStart.Add(9 * time.Hour), EndedAt: weekStart.Add(10 * time.Hour),
		},
		{
			DeviceID: 7, AppName: testFirefoxLower,
			StartedAt: weekStart.Add(32 * time.Hour), EndedAt: weekStart.Add(33 * time.Hour),
		},
	}

	chart := layoutFocusWeek(all, focus{records: all[:1], fill: testFocusFill}, devices, weekStart, weekEnd)

	if len(chart.HourMarks) != 7 {
		t.Errorf("axis cells = %d, want one per day", len(chart.HourMarks))
	}
	muted, focused := countBars(&chart)
	if muted != 2 || focused != 1 {
		t.Errorf("week chart = %d muted, %d focused; want 2 and 1", muted, focused)
	}
}
