package db

import (
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/identity"
)

const (
	chromeMacName   = ChromeAppName
	chromeLinuxName = "google-chrome"
	testGhosttyApp  = "Ghostty"
)

// Chrome tab coverage must never erase desktop Firefox time, or a day spent in
// one browser would delete the other browser from the chart.
func TestCoverageIsScopedToItsBrowserFamily(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	url := testActivityURL
	records := []ActivityRecordRow{
		// Chrome reports a full ten minutes of tab activity.
		browserRecord(1, 1, identity.ProducerChrome, chromeMacName, &url, start, start.Add(10*time.Minute)),
		// The native detector saw Firefox for the same ten minutes.
		browserRecord(2, 1, identity.ProducerDesktop, testFirefoxApp, nil, start, start.Add(10*time.Minute)),
	}

	totals := newActivityDeduplicator(records).totals(start, start.Add(10*time.Minute))
	if len(totals) != 2 {
		t.Fatalf("totals = %+v, want Chrome and Firefox both present", totals)
	}
	for _, total := range totals {
		if total.Seconds != 600 {
			t.Errorf("%s = %d seconds, want the full 600", total.AppName, total.Seconds)
		}
	}
}

// A browser's own coverage still subtracts its own desktop observation.
func TestChromeCoverageSubtractsDesktopChrome(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	url := testActivityURL
	records := []ActivityRecordRow{
		browserRecord(1, 1, identity.ProducerDesktop, chromeMacName, nil, start, start.Add(10*time.Minute)),
		browserRecord(2, 1, identity.ProducerChrome, chromeMacName, &url, start.Add(2*time.Minute), start.Add(8*time.Minute)),
	}

	totals := newActivityDeduplicator(records).totals(start, start.Add(10*time.Minute))
	if len(totals) != 1 {
		t.Fatalf("totals = %+v, want one Chrome row", totals)
	}
	if totals[0] != (AppTotalRow{AppName: chromeMacName, Seconds: 600}) {
		t.Errorf("total = %+v, want Google Chrome 600 with no double count", totals[0])
	}
}

// The Linux detector reports "google-chrome" for the same application the macOS
// detector calls "Google Chrome". Both must yield to Chrome tab coverage.
func TestChromeCoverageSubtractsTheLinuxAlias(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	url := testActivityURL
	records := []ActivityRecordRow{
		browserRecord(1, 1, identity.ProducerDesktop, chromeLinuxName, nil, start, start.Add(10*time.Minute)),
		browserRecord(2, 1, identity.ProducerChrome, chromeMacName, &url, start, start.Add(10*time.Minute)),
	}

	totals := newActivityDeduplicator(records).totals(start, start.Add(10*time.Minute))
	if len(totals) != 1 {
		t.Fatalf("totals = %+v, want one Chrome row", totals)
	}
	if totals[0].Seconds != 600 {
		t.Errorf("total = %+v, want 600 seconds counted once", totals[0])
	}
}

// All three Chrome spellings aggregate under the canonical display name.
func TestChromeAliasesAggregateIntoOneTotal(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	url := testActivityURL
	records := []ActivityRecordRow{
		browserRecord(1, 1, identity.ProducerChrome, chromeMacName, &url, start, start.Add(time.Minute)),
		browserRecord(2, 1, identity.ProducerDesktop, chromeLinuxName, nil, start.Add(2*time.Minute), start.Add(3*time.Minute)),
		browserRecord(3, 1, identity.ProducerDesktop, "google chrome", nil, start.Add(4*time.Minute), start.Add(5*time.Minute)),
	}

	totals := newActivityDeduplicator(records).totals(start, start.Add(10*time.Minute))
	if len(totals) != 1 {
		t.Fatalf("totals = %+v, want one canonical Chrome row", totals)
	}
	if totals[0] != (AppTotalRow{AppName: chromeMacName, Seconds: 180}) {
		t.Errorf("total = %+v, want Google Chrome 180", totals[0])
	}
}

// Coverage never crosses a device, whatever the browser.
func TestCoverageNeverCrossesDevices(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	url := testActivityURL
	records := []ActivityRecordRow{
		browserRecord(1, 1, identity.ProducerChrome, chromeMacName, &url, start, start.Add(10*time.Minute)),
		browserRecord(2, 2, identity.ProducerDesktop, chromeMacName, nil, start, start.Add(10*time.Minute)),
	}

	totals := newActivityDeduplicator(records).totals(start, start.Add(10*time.Minute))
	if len(totals) != 1 {
		t.Fatalf("totals = %+v, want one Chrome row", totals)
	}
	// Both devices contribute their full ten minutes: 1200 seconds.
	if totals[0].Seconds != 1200 {
		t.Errorf("total = %d seconds, want 1200 with no cross-device subtraction", totals[0].Seconds)
	}
}

// A URL-bearing record from an unknown producer renders normally and subtracts
// nothing -- it cannot claim a browser's coverage by naming itself one.
func TestUnknownProducerCoversNothing(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	url := testActivityURL
	records := []ActivityRecordRow{
		browserRecord(1, 1, identity.ProducerDesktop, "Safari", &url, start, start.Add(10*time.Minute)),
		browserRecord(2, 1, identity.ProducerDesktop, testFirefoxApp, nil, start, start.Add(10*time.Minute)),
	}

	totals := newActivityDeduplicator(records).totals(start, start.Add(10*time.Minute))
	if len(totals) != 2 {
		t.Fatalf("totals = %+v, want Safari and Firefox untouched", totals)
	}
	for _, total := range totals {
		if total.Seconds != 600 {
			t.Errorf("%s = %d seconds, want 600", total.AppName, total.Seconds)
		}
	}
}

func TestCanonicalAppNameKeepsUnknownNames(t *testing.T) {
	t.Parallel()

	record := ActivityRecordRow{Producer: identity.ProducerDesktop, AppName: testGhosttyApp}
	if got := CanonicalAppName(&record); got != testGhosttyApp {
		t.Errorf("CanonicalAppName = %q, want the stored name", got)
	}
}
