package db

import (
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/identity"
)

// firefoxAppKey is the normalized Linux WM_CLASS name; the macOS detector
// reports "Firefox". Both belong to the same browser family.
const firefoxAppKey = "firefox"

const testActivityURL = "https://example.com/"

func TestDeduplicateFirefoxActivitySplitsDesktopRecord(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	url := testActivityURL
	records := []ActivityRecordRow{
		activityRecord(1, 1, firefoxAppKey, nil, start, start.Add(10*time.Minute)),
		activityRecord(2, 1, testFirefoxApp, &url, start.Add(2*time.Minute), start.Add(8*time.Minute)),
	}

	got := deduplicateActivityForTest(t, records)
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

	got := deduplicateActivityForTest(t, records)
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

	got := deduplicateActivityForTest(t, records)
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

	got := deduplicateActivityForTest(t, records)
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

	deduplicator := newActivityDeduplicator(records)
	got, truncated := deduplicator.timeline(1000, 10000)
	if truncated {
		t.Fatal("test fixture unexpectedly exceeded deduplication bounds")
	}
	if len(got) != 1 || got[0].URL == nil {
		t.Errorf("records = %+v, want only the browser observation", got)
	}
	totals := deduplicator.totals(start, start.Add(10*time.Second))
	if len(totals) != 1 || totals[0] != (AppTotalRow{AppName: testFirefoxApp, Seconds: 9}) {
		t.Errorf("totals = %+v, want only the visible browser observation", totals)
	}
}

func TestAppTotalsUsesEffectiveSlices(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	url := testActivityURL
	records := []ActivityRecordRow{
		activityRecord(1, 1, firefoxAppKey, nil, start.Add(-time.Minute), start.Add(10*time.Minute)),
		activityRecord(2, 1, testFirefoxApp, &url, start.Add(2*time.Minute), start.Add(8*time.Minute)),
	}

	// "Firefox" and "firefox" are one browser to a reader, so the browser's
	// 360 seconds and the desktop residue's 180 aggregate into one row.
	got := newActivityDeduplicator(records).totals(start, start.Add(9*time.Minute))
	if len(got) != 1 {
		t.Fatalf("totals = %+v, want one canonical Firefox row", got)
	}
	if got[0] != (AppTotalRow{AppName: testFirefoxApp, Seconds: 540}) {
		t.Errorf("total = %+v, want Firefox 540", got[0])
	}
}

func TestActivityDeduplicationBoundsExpansionWork(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	url := testActivityURL
	records := make([]ActivityRecordRow, 0, 21)
	records = append(records, activityRecord(1, 1, firefoxAppKey, nil, start, start.Add(time.Hour)))
	for i := range 20 {
		intervalStart := start.Add(time.Duration(i*2+1) * time.Minute)
		records = append(records, activityRecord(
			int64(i+2),
			1,
			testFirefoxApp,
			&url,
			intervalStart,
			intervalStart.Add(time.Minute),
		))
	}

	deduplicator := newActivityDeduplicator(records)
	got, truncated := deduplicator.timeline(100, 5)
	if !truncated {
		t.Fatal("timeline was not truncated after exhausting its work budget")
	}
	if len(got) > 100 {
		t.Errorf("records = %d, want at most 100", len(got))
	}
	totals := deduplicator.totals(start, start.Add(time.Hour))
	if len(totals) != 1 {
		t.Fatalf("totals = %+v, want one canonical Firefox row", totals)
	}
	if totals[0].Seconds != int64(time.Hour/time.Second) {
		t.Errorf("totals = %+v, want exact one-hour coverage", totals)
	}
}

func TestActivityDeduplicationDropsInvalidRecords(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	records := []ActivityRecordRow{
		activityRecord(1, 1, "Valid", nil, start, start.Add(time.Minute)),
		activityRecord(2, 1, "Zero", nil, start, start),
		activityRecord(3, 1, "Negative", nil, start, start.Add(-time.Minute)),
	}

	deduplicator := newActivityDeduplicator(records)
	got, truncated := deduplicator.timeline(10, 10)
	if truncated {
		t.Fatal("invalid records unexpectedly caused truncation")
	}
	if len(got) != 1 || got[0].AppName != "Valid" {
		t.Errorf("records = %+v, want only the valid interval", got)
	}
	if totals := deduplicator.totals(start, start.Add(time.Hour)); len(totals) != 1 || totals[0].AppName != "Valid" {
		t.Errorf("totals = %+v, want only the valid interval", totals)
	}
}

func TestVisibleUncoveredDurationDropsInternalSubsecondGaps(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	coverage := activityCoverage{
		intervals: []activityInterval{
			{start: start.Add(time.Second), end: start.Add(3 * time.Second)},
			{start: start.Add(3500 * time.Millisecond), end: start.Add(5 * time.Second)},
			{start: start.Add(7 * time.Second), end: start.Add(9 * time.Second)},
		},
		visibleGapPrefix: []time.Duration{0, 0, 2 * time.Second},
	}

	got := coverage.visibleUncoveredDuration(
		activityInterval{start: start, end: start.Add(10 * time.Second)},
		minEffectiveSliceDuration,
	)
	if got != 4*time.Second {
		t.Errorf("visible uncovered duration = %s, want 4s", got)
	}
}

func deduplicateActivityForTest(t *testing.T, records []ActivityRecordRow) []ActivityRecordRow {
	t.Helper()
	got, truncated := newActivityDeduplicator(records).timeline(1000, 10000)
	if truncated {
		t.Fatal("test fixture unexpectedly exceeded deduplication bounds")
	}
	return got
}

// activityRecord builds a record the way the pre-Chrome pipeline did: a URL
// means the Firefox extension reported it, anything else came from the native
// detector. Cases that care about the producer set it explicitly with
// browserRecord instead.
func activityRecord(id, deviceID int64, appName string, url *string, start, end time.Time) ActivityRecordRow {
	producer := identity.ProducerDesktop
	if url != nil && *url != "" {
		producer = identity.ProducerFirefox
	}
	return browserRecord(id, deviceID, producer, appName, url, start, end)
}

func browserRecord(
	id, deviceID int64,
	producer identity.Producer,
	appName string,
	url *string,
	start, end time.Time,
) ActivityRecordRow {
	return ActivityRecordRow{
		ID:        id,
		DeviceID:  deviceID,
		Producer:  producer,
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
