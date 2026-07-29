package db

import (
	"testing"
	"time"
)

const testActivityURL = "https://example.com/"

func TestDeduplicateFirefoxActivitySplitsDesktopRecord(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	url := testActivityURL
	records := []ActivityRecordRow{
		activityRecord(1, 1, firefoxAppKey, nil, start, start.Add(10*time.Minute)),
		activityRecord(2, 1, testFirefoxApp, &url, start.Add(2*time.Minute), start.Add(8*time.Minute)),
	}

	got := deduplicateFirefoxActivity(records)
	if len(got) != 3 {
		t.Fatalf("records = %+v, want three effective slices", got)
	}
	assertActivityInterval(t, &got[0], start, start.Add(2*time.Minute))
	assertActivityInterval(t, &got[1], start.Add(2*time.Minute), start.Add(8*time.Minute))
	assertActivityInterval(t, &got[2], start.Add(8*time.Minute), start.Add(10*time.Minute))
	if got[0].AppName != firefoxAppKey || got[1].AppName != testFirefoxApp || got[2].AppName != firefoxAppKey {
		t.Errorf("app sequence = %q, %q, %q", got[0].AppName, got[1].AppName, got[2].AppName)
	}
}

func TestDeduplicateFirefoxActivityMergesBrowserCoverage(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	firstURL := "https://example.com/first"
	secondURL := "https://example.com/second"
	records := []ActivityRecordRow{
		activityRecord(1, 1, firefoxAppKey, nil, start, start.Add(20*time.Minute)),
		activityRecord(2, 1, testFirefoxApp, &firstURL, start.Add(2*time.Minute), start.Add(9*time.Minute)),
		activityRecord(3, 1, testFirefoxApp, &secondURL, start.Add(7*time.Minute), start.Add(15*time.Minute)),
	}

	got := deduplicateFirefoxActivity(records)
	if len(got) != 4 {
		t.Fatalf("records = %+v, want two browser records and two desktop slices", got)
	}
	assertActivityInterval(t, &got[0], start, start.Add(2*time.Minute))
	assertActivityInterval(t, &got[3], start.Add(15*time.Minute), start.Add(20*time.Minute))
}

func TestDeduplicateFirefoxActivityIsDeviceScoped(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	url := testActivityURL
	records := []ActivityRecordRow{
		activityRecord(1, 1, firefoxAppKey, nil, start, start.Add(10*time.Minute)),
		activityRecord(2, 2, testFirefoxApp, &url, start, start.Add(10*time.Minute)),
		activityRecord(3, 1, "Safari", nil, start, start.Add(10*time.Minute)),
	}

	got := deduplicateFirefoxActivity(records)
	if len(got) != len(records) {
		t.Fatalf("records = %+v, want all records preserved", got)
	}
}

func TestDeduplicateFirefoxActivityKeepsTouchingIntervals(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	url := testActivityURL
	records := []ActivityRecordRow{
		activityRecord(1, 1, firefoxAppKey, nil, start, start.Add(10*time.Minute)),
		activityRecord(2, 1, testFirefoxApp, &url, start.Add(10*time.Minute), start.Add(20*time.Minute)),
	}

	got := deduplicateFirefoxActivity(records)
	if len(got) != len(records) {
		t.Fatalf("records = %+v, want touching records preserved", got)
	}
}

func TestDeduplicateFirefoxActivityDropsSubsecondDesktopSlices(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	url := testActivityURL
	records := []ActivityRecordRow{
		activityRecord(1, 1, firefoxAppKey, nil, start, start.Add(10*time.Second)),
		activityRecord(
			2,
			1,
			testFirefoxApp,
			&url,
			start.Add(500*time.Millisecond),
			start.Add(9500*time.Millisecond),
		),
	}

	got := deduplicateFirefoxActivity(records)
	if len(got) != 1 || got[0].URL == nil {
		t.Errorf("records = %+v, want only the browser observation", got)
	}
}

func TestAppTotalsUsesEffectiveSlices(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	url := testActivityURL
	records := deduplicateFirefoxActivity([]ActivityRecordRow{
		activityRecord(1, 1, firefoxAppKey, nil, start.Add(-time.Minute), start.Add(10*time.Minute)),
		activityRecord(2, 1, testFirefoxApp, &url, start.Add(2*time.Minute), start.Add(8*time.Minute)),
	})

	got := appTotals(records, start, start.Add(9*time.Minute))
	if len(got) != 2 {
		t.Fatalf("totals = %+v, want browser and desktop rows", got)
	}
	if got[0] != (AppTotalRow{AppName: testFirefoxApp, Seconds: 360}) {
		t.Errorf("first total = %+v, want Firefox 360", got[0])
	}
	if got[1] != (AppTotalRow{AppName: firefoxAppKey, Seconds: 180}) {
		t.Errorf("second total = %+v, want firefox 180", got[1])
	}
}

func activityRecord(id, deviceID int64, appName string, url *string, start, end time.Time) ActivityRecordRow {
	return ActivityRecordRow{
		ID:        id,
		DeviceID:  deviceID,
		AppName:   appName,
		URL:       url,
		StartedAt: start,
		EndedAt:   end,
		DurationS: int(end.Sub(start).Seconds()),
	}
}

func assertActivityInterval(t *testing.T, got *ActivityRecordRow, wantStart, wantEnd time.Time) {
	t.Helper()
	if !got.StartedAt.Equal(wantStart) || !got.EndedAt.Equal(wantEnd) {
		t.Errorf("interval = [%s, %s), want [%s, %s)", got.StartedAt, got.EndedAt, wantStart, wantEnd)
	}
	if got.DurationS != int(wantEnd.Sub(wantStart).Seconds()) {
		t.Errorf("duration = %d, want %d", got.DurationS, int(wantEnd.Sub(wantStart).Seconds()))
	}
}
