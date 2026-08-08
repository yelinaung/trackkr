package db

import (
	"cmp"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/identity"
	"hegel.dev/go/hegel"
)

// testDedupEpoch anchors every drawn interval so a counterexample reads
// as an offset from one instant instead of an absolute timestamp.
var testDedupEpoch = time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)

// drawInterval draws an interval from a deliberately small span.
//
// The span is narrow so overlaps and exact coincidences are common: the
// merge rule turns on whether one interval starts exactly where another
// ended, and a wide draw would almost never produce that. Durations
// start at zero because mergeActivityIntervals accepts a point interval
// even though the record filter upstream would not.
func drawInterval(tc hegel.TestCase) activityInterval {
	startMs := hegel.Draw(tc, hegel.Integers(0, 600_000))
	durationMs := hegel.Draw(tc, hegel.Integers(0, 300_000))
	start := testDedupEpoch.Add(time.Duration(startMs) * time.Millisecond)
	return activityInterval{start: start, end: start.Add(time.Duration(durationMs) * time.Millisecond)}
}

func drawIntervals(tc hegel.TestCase) []activityInterval {
	count := hegel.Draw(tc, hegel.Integers(0, 40))
	intervals := make([]activityInterval, count)
	for i := range intervals {
		intervals[i] = drawInterval(tc)
	}
	return intervals
}

// drawCanonicalCoverage builds coverage the way newActivityDeduplicator
// does, because that is the only shape its consumers accept.
//
// visitUncoveredActivityIntervals binary-searches the slice, so unsorted
// input makes it report a gap that is not there, and
// visibleUncoveredDuration indexes visibleGapPrefix, so a nil prefix
// panics rather than answering. Neither is a defect the production code
// can reach: both read coverage this constructor built.
func drawCanonicalCoverage(tc hegel.TestCase) activityCoverage {
	merged := mergeActivityIntervals(drawIntervals(tc))
	prefix := make([]time.Duration, len(merged))
	for i := 0; i+1 < len(merged); i++ {
		prefix[i+1] = prefix[i]
		if gap := merged[i+1].start.Sub(merged[i].end); gap >= minEffectiveSliceDuration {
			prefix[i+1] += gap
		}
	}
	return activityCoverage{intervals: merged, visibleGapPrefix: prefix}
}

// naiveUnion merges by repeated pairwise absorption until nothing
// changes. It is quadratic and deliberately unlike the sort-and-sweep
// under test, so agreeing with it means something.
func naiveUnion(intervals []activityInterval) []activityInterval {
	union := slices.Clone(intervals)
	for changed := true; changed; {
		changed = false
		for i := 0; i < len(union) && !changed; i++ {
			for j := i + 1; j < len(union); j++ {
				if union[i].end.Before(union[j].start) || union[j].end.Before(union[i].start) {
					continue
				}
				union[i] = activityInterval{
					start: minTime(union[i].start, union[j].start),
					end:   maxTime(union[i].end, union[j].end),
				}
				union = append(union[:j], union[j+1:]...)
				changed = true
				break
			}
		}
	}
	slices.SortFunc(union, func(a, b activityInterval) int { return a.start.Compare(b.start) })
	return union
}

// overlapWithin totals how much of coverage falls inside subject,
// assuming coverage is already disjoint.
func overlapWithin(subject activityInterval, coverage []activityInterval) time.Duration {
	var total time.Duration
	for _, interval := range coverage {
		start := maxTime(interval.start, subject.start)
		end := minTime(interval.end, subject.end)
		if start.Before(end) {
			total += end.Sub(start)
		}
	}
	return total
}

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

func TestCategoryTotalsUseLargestRemainderWithUncategorizedTieBreak(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	workID := int64(5)
	records := []ActivityRecordRow{
		activityRecord(1, 1, "Code", nil, start, start.Add(600*time.Millisecond)),
		activityRecord(2, 1, "Code", nil, start.Add(time.Second), start.Add(1600*time.Millisecond)),
	}
	records[0].CategoryOverridePresent = true
	records[0].CategoryOverrideID = &workID

	deduplicator := newActivityDeduplicator(records)
	apps := deduplicator.totals(start, start.Add(2*time.Second))
	categories := deduplicator.categoryTotals(start, start.Add(2*time.Second), nil, map[int64]CategoryRow{
		workID: {ID: workID, Name: "Work", ColorKey: "sky"},
	})
	if len(apps) != 1 || apps[0].Seconds != 1 {
		t.Fatalf("application totals = %+v, want Code 1", apps)
	}
	if len(categories) != 1 || categories[0].Name != UncategorizedCategoryName || categories[0].Seconds != apps[0].Seconds {
		t.Errorf("category totals = %+v, want Uncategorized to receive the tied remainder", categories)
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

// TestMergeActivityIntervalsPreservesUnion asserts four readings of one
// output together, because splitting them would duplicate the generator
// and the quadratic reference for no gain.
//
// Non-adjacency is a contract, not a universal truth about merge
// functions. The condition is interval.start.After(previous.end), so an
// interval starting exactly where the previous one ended is absorbed,
// which is what turns a browser reporting back-to-back one-second tabs
// into a single coverage span instead of thousands.
func TestMergeActivityIntervalsPreservesUnion(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		intervals := drawIntervals(ht)
		merged := mergeActivityIntervals(slices.Clone(intervals))

		for i := range merged {
			if merged[i].end.Before(merged[i].start) {
				ht.Fatalf("merged[%d] = [%s, %s) runs backwards",
					i, merged[i].start, merged[i].end)
			}
			if i == 0 {
				continue
			}
			if merged[i].start.Before(merged[i-1].start) {
				ht.Fatalf("merged is not sorted at %d: %s before %s",
					i, merged[i].start, merged[i-1].start)
			}
			if !merged[i].start.After(merged[i-1].end) {
				ht.Fatalf("merged[%d] starts at %s, not after the previous end %s,"+
					" so two spans that touch or overlap survived",
					i, merged[i].start, merged[i-1].end)
			}
		}

		want := naiveUnion(intervals)
		if len(merged) != len(want) {
			ht.Fatalf("merged %d intervals from %d inputs, reference found %d",
				len(merged), len(intervals), len(want))
		}
		for i := range want {
			if !merged[i].start.Equal(want[i].start) || !merged[i].end.Equal(want[i].end) {
				ht.Fatalf("merged[%d] = [%s, %s), reference = [%s, %s)",
					i, merged[i].start, merged[i].end, want[i].start, want[i].end)
			}
		}
	})
}

// TestVisitUncoveredActivityIntervalsPartitionsTheSubject pins the
// cursor advance, which is the subtlest loop in the file. The visited
// slices must tile exactly the part of the subject no coverage claims.
func TestVisitUncoveredActivityIntervalsPartitionsTheSubject(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		subject := drawInterval(ht)
		coverage := drawCanonicalCoverage(ht)

		work := 0
		var visited []activityInterval
		completed := visitUncoveredActivityIntervals(
			subject,
			coverage.intervals,
			&work,
			math.MaxInt32,
			func(interval activityInterval) { visited = append(visited, interval) },
		)
		if !completed {
			ht.Fatalf("visit stopped early despite an effectively unlimited budget")
		}

		var total time.Duration
		for i, interval := range visited {
			if interval.start.Before(subject.start) || interval.end.After(subject.end) {
				ht.Fatalf("visited[%d] = [%s, %s) escapes the subject [%s, %s)",
					i, interval.start, interval.end, subject.start, subject.end)
			}
			if i > 0 && interval.start.Before(visited[i-1].end) {
				ht.Fatalf("visited[%d] starts at %s, before the previous end %s",
					i, interval.start, visited[i-1].end)
			}
			if covered := overlapWithin(interval, coverage.intervals); covered != 0 {
				ht.Fatalf("visited[%d] = [%s, %s) overlaps %s of coverage",
					i, interval.start, interval.end, covered)
			}
			total += interval.end.Sub(interval.start)
		}

		subjectDuration := max(subject.end.Sub(subject.start), 0)
		if want := subjectDuration - overlapWithin(subject, coverage.intervals); total != want {
			ht.Fatalf("visited %s of the subject, want %s uncovered", total, want)
		}
	})
}

// TestVisitUncoveredActivityIntervalsRespectsTheWorkLimit guards the
// bound that keeps adversarial overlaps from expanding quadratically.
// A generator supplies the adversarial inputs for free.
func TestVisitUncoveredActivityIntervalsRespectsTheWorkLimit(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		subject := drawInterval(ht)
		coverage := drawCanonicalCoverage(ht)
		workLimit := hegel.Draw(ht, hegel.Integers(0, 50))

		work := 0
		completed := visitUncoveredActivityIntervals(
			subject, coverage.intervals, &work, workLimit, func(activityInterval) {},
		)
		if work > workLimit {
			ht.Fatalf("work reached %d, past the limit of %d", work, workLimit)
		}
		if !completed && work != workLimit {
			ht.Fatalf("visit reported truncation at work %d, below the limit of %d", work, workLimit)
		}

		generous := 0
		if !visitUncoveredActivityIntervals(
			subject, coverage.intervals, &generous, len(coverage.intervals)+1, func(activityInterval) {},
		) {
			ht.Fatalf("visit truncated with a budget of %d for %d coverage intervals",
				len(coverage.intervals)+1, len(coverage.intervals))
		}
	})
}

// TestVisibleUncoveredDurationMatchesMaterializedSlices is the oracle
// test the batch exists for. totals sums from a prefix table and a
// binary search; timeline materializes each slice. Both are in this
// file, both claim the same number, and nothing compared them.
func TestVisibleUncoveredDurationMatchesMaterializedSlices(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		subject := drawInterval(ht)
		coverage := drawCanonicalCoverage(ht)

		work := 0
		var materialized time.Duration
		if !visitUncoveredActivityIntervals(
			subject,
			coverage.intervals,
			&work,
			math.MaxInt32,
			func(interval activityInterval) {
				if slice := interval.end.Sub(interval.start); slice >= minEffectiveSliceDuration {
					materialized += slice
				}
			},
		) {
			ht.Fatalf("visit stopped early despite an effectively unlimited budget")
		}

		summed := coverage.visibleUncoveredDuration(subject, minEffectiveSliceDuration)
		if summed != materialized {
			ht.Fatalf("visibleUncoveredDuration = %s, materialized slices total %s"+
				" over subject [%s, %s) and %d coverage intervals",
				summed, materialized, subject.start, subject.end, len(coverage.intervals))
		}
	})
}

// boundedRecordsMachine drives the heap against a plain slice.
type boundedRecordsMachine struct {
	limit   int // already normalized the way the constructor normalizes it
	bounded *boundedActivityRecords
	model   []*ActivityRecordRow
	nextID  int64
}

// RuleAdd offers one record to both the heap and the model.
//
// StartedAt comes from a six-value pool and DeviceID from three, so ties
// at the first and second levels of compareActivityRecords are common
// and the later comparisons decide the order. IDs stay unique, which
// keeps the comparator a total order and the expected top-k unambiguous.
func (m *boundedRecordsMachine) RuleAdd(tc hegel.TestCase) {
	m.nextID++
	start := testDedupEpoch.Add(
		time.Duration(hegel.Draw(tc, hegel.Integers(0, 5))) * time.Second,
	)
	record := browserRecord(
		m.nextID,
		hegel.Draw(tc, hegel.Integers[int64](1, 3)),
		identity.ProducerDesktop,
		"App",
		nil,
		start,
		start.Add(time.Minute),
	)
	m.bounded.add(&record)
	m.model = append(m.model, &record)
}

// InvariantKeepsTheSmallest checks the heap against the model's own
// top-k, recomputed from scratch after every rule.
func (m *boundedRecordsMachine) InvariantKeepsTheSmallest(tc hegel.TestCase) {
	want := slices.Clone(m.model)
	slices.SortFunc(want, compareActivityRecords)
	if len(want) > m.limit {
		want = want[:m.limit]
	}

	got := m.bounded.sorted()
	if len(got) != len(want) {
		tc.Errorf("kept %d records, want %d with limit %d after %d adds",
			len(got), len(want), m.limit, len(m.model))
		tc.FailNow()
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			tc.Errorf("kept record %d at position %d, want %d", got[i].ID, i, want[i].ID)
			tc.FailNow()
		}
	}
}

// InvariantFlagsRejection ties truncated to a rejected add rather than
// to a zero limit. A limiter built with zero starts out false and only
// sets the flag inside add, so the invariant has to hold for a fresh
// machine too -- RunStateful checks it before any rule runs.
func (m *boundedRecordsMachine) InvariantFlagsRejection(tc hegel.TestCase) {
	if rejected := len(m.model) > m.limit; rejected != m.bounded.truncated {
		tc.Errorf("truncated = %v after %d adds with limit %d",
			m.bounded.truncated, len(m.model), m.limit)
		tc.FailNow()
	}
}

func TestBoundedActivityRecordsKeepsTheSmallest(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		// Negative limits normalize to zero in the constructor, so the
		// draw reaches below zero to exercise that.
		limit := hegel.Draw(ht, hegel.Integers(-2, 5))
		hegel.RunStateful(ht, &boundedRecordsMachine{
			limit:   max(0, limit),
			bounded: newBoundedActivityRecords(limit),
		})
	})
}

// drawCategorizedRecords draws desktop records for the named
// applications, each routed to a category or left uncategorized.
//
// Durations carry millisecond fractions because largest remainder only
// does anything when the seconds do not divide evenly. The count and
// duration caps together keep one application's accumulation near 8.3
// days at worst, an order of magnitude under the 2^53 ns ceiling where
// applicationDuration.Seconds() stops being exact -- past that the
// partition fails for a float reason that says nothing about the
// allocation. Real query windows are days and weeks, so the ceiling is
// unreachable outside this test.
func drawCategorizedRecords(tc hegel.TestCase, appNames []string) []ActivityRecordRow {
	count := hegel.Draw(tc, hegel.Integers(0, 200))
	records := make([]ActivityRecordRow, 0, count)
	for i := range count {
		startMs := hegel.Draw(tc, hegel.Integers(0, 600_000))
		durationMs := hegel.Draw(tc, hegel.Integers(1, 3_600_000))
		start := testDedupEpoch.Add(time.Duration(startMs) * time.Millisecond)
		record := browserRecord(
			int64(i+1),
			1,
			identity.ProducerDesktop,
			hegel.Draw(tc, hegel.SampledFrom(appNames)),
			nil,
			start,
			start.Add(time.Duration(durationMs)*time.Millisecond),
		)

		if categoryID := hegel.Draw(tc, hegel.Integers[int64](0, 3)); categoryID != 0 {
			record.CategoryOverridePresent = true
			record.CategoryOverrideID = &categoryID
		}
		records = append(records, record)
	}
	return records
}

// testCategoryApp is one application name the category properties draw
// records for; the literal is shared so goconst stays quiet.
const testCategoryApp = "Code"

var testCategoryRows = map[int64]CategoryRow{
	1: {ID: 1, Name: "Focus", ColorKey: "sky"},
	2: {ID: 2, Name: "Reading", ColorKey: "amber"},
	3: {ID: 3, Name: "Chat", ColorKey: "rose"},
}

// TestCategoryTotalsPartitionOneApplication is the tight form of the
// largest-remainder claim. With one application in play, the returned
// rows are that application's allocation and nothing else, so the
// published category seconds must add up to its published total.
func TestCategoryTotalsPartitionOneApplication(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		records := drawCategorizedRecords(ht, []string{testCategoryApp})
		start, end := testDedupEpoch.Add(-time.Hour), testDedupEpoch.Add(4*time.Hour)

		deduplicator := newActivityDeduplicator(records)
		apps := deduplicator.totals(start, end)
		categories := deduplicator.categoryTotals(start, end, nil, testCategoryRows)

		var allocated int64
		for _, category := range categories {
			allocated += category.Seconds
		}

		var want int64
		if len(apps) > 0 {
			want = apps[0].Seconds
		}
		if allocated != want {
			ht.Fatalf("category seconds total %d over %d records, want the application's %d",
				allocated, len(records), want)
		}
	})
}

// modelCategorySeconds allocates the drawn records independently of the
// code under test, so the properties can check where time landed rather
// than only how much of it there was.
//
// It reproduces the destination rule -- an override wins, an unknown
// override falls through to uncategorized -- and then sums whole seconds
// per destination with the fractional remainders resolved largest first,
// ties going to the lower category ID. Rounding is per application,
// which is the level categoryTotals allocates at.
func modelCategorySeconds(records []ActivityRecordRow) map[int64]int64 {
	perApp := make(map[string]map[int64]time.Duration)
	for i := range records {
		record := &records[i]
		if !isValidActivityRecord(record) {
			continue
		}
		var destination int64
		if record.CategoryOverridePresent && record.CategoryOverrideID != nil {
			if _, known := testCategoryRows[*record.CategoryOverrideID]; known {
				destination = *record.CategoryOverrideID
			}
		}
		appName := CanonicalAppName(record)
		if perApp[appName] == nil {
			perApp[appName] = make(map[int64]time.Duration)
		}
		perApp[appName][destination] += record.EndedAt.Sub(record.StartedAt)
	}

	seconds := make(map[int64]int64)
	for _, durations := range perApp {
		type share struct {
			destination int64
			remainder   time.Duration
		}
		var total time.Duration
		var whole int64
		shares := make([]share, 0, len(durations))
		for destination, duration := range durations {
			total += duration
			seconds[destination] += int64(duration / time.Second)
			whole += int64(duration / time.Second)
			shares = append(shares, share{destination, duration % time.Second})
		}
		slices.SortFunc(shares, func(a, b share) int {
			if order := cmp.Compare(b.remainder, a.remainder); order != 0 {
				return order
			}
			return cmp.Compare(a.destination, b.destination)
		})
		for i := range int64(math.Round(total.Seconds())) - whole {
			seconds[shares[i].destination]++
		}
	}
	return seconds
}

// TestCategoryTotalsAllocateToTheRightCategory is what conservation
// alone cannot catch: the seconds could add up while every record landed
// under the wrong name. It compares each published row against the model
// above, destination by destination.
func TestCategoryTotalsAllocateToTheRightCategory(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		records := drawCategorizedRecords(ht, []string{testCategoryApp, "Mail", "Notes"})
		start, end := testDedupEpoch.Add(-time.Hour), testDedupEpoch.Add(4*time.Hour)

		want := modelCategorySeconds(records)
		for _, row := range newActivityDeduplicator(records).categoryTotals(
			start, end, nil, testCategoryRows,
		) {
			var destination int64
			if row.CategoryID != nil {
				destination = *row.CategoryID
			}

			wantName := UncategorizedCategoryName
			if destination != 0 {
				wantName = testCategoryRows[destination].Name
			}
			if row.Name != wantName {
				ht.Fatalf("category %d published as %q, want %q", destination, row.Name, wantName)
			}
			if row.Seconds != want[destination] {
				ht.Fatalf("category %q published %d seconds over %d records, model says %d",
					row.Name, row.Seconds, len(records), want[destination])
			}
			delete(want, destination)
		}

		for destination, seconds := range want {
			if seconds != 0 {
				ht.Fatalf("category %d earned %d seconds but was not published",
					destination, seconds)
			}
		}
	})
}

// TestCategoryTotalsPartitionEveryApplication is the claim that survives
// several applications. categoryTotals sums into one row per category
// across all of them, so a per-application partition is no longer
// observable from the return value -- the identity that is, and the one
// a reader sees on the dashboard, is that the category column adds up to
// the application column.
func TestCategoryTotalsPartitionEveryApplication(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		records := drawCategorizedRecords(ht, []string{testCategoryApp, "Mail", "Notes", "Terminal"})
		start, end := testDedupEpoch.Add(-time.Hour), testDedupEpoch.Add(4*time.Hour)

		deduplicator := newActivityDeduplicator(records)

		var want int64
		for _, app := range deduplicator.totals(start, end) {
			want += app.Seconds
		}
		var allocated int64
		for _, category := range deduplicator.categoryTotals(start, end, nil, testCategoryRows) {
			allocated += category.Seconds
		}
		if allocated != want {
			ht.Fatalf("category seconds total %d over %d records, want the application total %d",
				allocated, len(records), want)
		}
	})
}

// TestTotalsAgreeWithTimeline compares the two paths end to end: the
// slices the chart draws, summed per canonical application, against the
// numbers the list publishes.
//
// The window contains every drawn record, so nothing is clipped and the
// two answer the same question. Records are a mix of Firefox tab
// observations and native Firefox windows on one device, which is the
// arrangement that makes coverage subtraction happen at all.
func TestTotalsAgreeWithTimeline(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers(0, 30))
		records := make([]ActivityRecordRow, 0, count)
		for i := range count {
			interval := drawInterval(ht)
			if hegel.Draw(ht, hegel.Booleans()) {
				url := testActivityURL
				records = append(records, browserRecord(
					int64(i+1), 1, identity.ProducerFirefox, testFirefoxApp, &url,
					interval.start, interval.end,
				))
				continue
			}
			records = append(records, browserRecord(
				int64(i+1), 1, identity.ProducerDesktop, firefoxAppKey, nil,
				interval.start, interval.end,
			))
		}

		start, end := testDedupEpoch.Add(-time.Hour), testDedupEpoch.Add(4*time.Hour)
		deduplicator := newActivityDeduplicator(records)

		drawn, truncated := deduplicator.timeline(10_000, 1_000_000)
		if truncated {
			ht.Fatalf("timeline truncated %d records under a generous budget", len(records))
		}

		byApp := make(map[string]time.Duration)
		for i := range drawn {
			record := &drawn[i]
			byApp[CanonicalAppName(record)] += record.EndedAt.Sub(record.StartedAt)
		}

		published := make(map[string]int64)
		for _, app := range deduplicator.totals(start, end) {
			published[app.AppName] = app.Seconds
		}
		for appName, duration := range byApp {
			want := int64(math.Round(duration.Seconds()))
			if published[appName] != want {
				ht.Fatalf("timeline shows %s of %q (%d seconds), totals publishes %d",
					duration, appName, want, published[appName])
			}
		}
		if len(published) != len(byApp) {
			ht.Fatalf("totals published %d applications, timeline drew %d",
				len(published), len(byApp))
		}
	})
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
