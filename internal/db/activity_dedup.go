package db

import (
	"cmp"
	"container/heap"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/yelinaung/trackkr/internal/icon"
)

const (
	firefoxAppKey             = "firefox"
	minEffectiveSliceDuration = time.Second
	activityDedupWorkLimit    = ActivitySourceLimit * 4
)

type activityInterval struct {
	start time.Time
	end   time.Time
}

type activityCoverage struct {
	intervals        []activityInterval
	visibleGapPrefix []time.Duration
}

type activityDeduplicator struct {
	records  []ActivityRecordRow
	coverage map[int64]activityCoverage
}

// newActivityDeduplicator gives URL-bearing Firefox observations precedence
// over desktop Firefox observations for the same device and time interval.
func newActivityDeduplicator(records []ActivityRecordRow) *activityDeduplicator {
	intervalsByDevice := make(map[int64][]activityInterval)
	for i := range records {
		record := &records[i]
		if isValidActivityRecord(record) && isFirefoxBrowserRecord(record) {
			intervalsByDevice[record.DeviceID] = append(
				intervalsByDevice[record.DeviceID],
				activityInterval{start: record.StartedAt, end: record.EndedAt},
			)
		}
	}

	coverage := make(map[int64]activityCoverage, len(intervalsByDevice))
	for deviceID, intervals := range intervalsByDevice {
		merged := mergeActivityIntervals(intervals)
		visibleGapPrefix := make([]time.Duration, len(merged))
		for i := 0; i+1 < len(merged); i++ {
			visibleGapPrefix[i+1] = visibleGapPrefix[i]
			gap := merged[i+1].start.Sub(merged[i].end)
			if gap >= minEffectiveSliceDuration {
				visibleGapPrefix[i+1] += gap
			}
		}
		coverage[deviceID] = activityCoverage{
			intervals:        merged,
			visibleGapPrefix: visibleGapPrefix,
		}
	}

	return &activityDeduplicator{records: records, coverage: coverage}
}

// timeline returns at most recordLimit effective records. workLimit bounds
// coverage subtraction so adversarial overlaps cannot expand quadratically.
func (d *activityDeduplicator) timeline(recordLimit, workLimit int) ([]ActivityRecordRow, bool) {
	bounded := newBoundedActivityRecords(recordLimit)
	for i := range d.records {
		record := &d.records[i]
		if !isValidActivityRecord(record) || isFirefoxDesktopRecord(record) {
			continue
		}
		bounded.add(record)
	}

	work := 0
	for i := range d.records {
		record := &d.records[i]
		if !isValidActivityRecord(record) || !isFirefoxDesktopRecord(record) {
			continue
		}

		completed := visitUncoveredActivityIntervals(
			activityInterval{start: record.StartedAt, end: record.EndedAt},
			d.coverage[record.DeviceID].intervals,
			&work,
			workLimit,
			func(interval activityInterval) {
				if interval.end.Sub(interval.start) < minEffectiveSliceDuration {
					return
				}
				slice := *record
				slice.StartedAt = interval.start
				slice.EndedAt = interval.end
				slice.DurationS = int(interval.end.Sub(interval.start).Seconds())
				bounded.add(&slice)
			},
		)
		if !completed {
			bounded.truncated = true
			break
		}
	}

	return bounded.sorted(), bounded.truncated
}

// totals computes effective durations directly from merged coverage. It does
// not materialize every desktop slice merely to sum it.
func (d *activityDeduplicator) totals(start, end time.Time) []AppTotalRow {
	durations := make(map[string]time.Duration)
	for i := range d.records {
		record := &d.records[i]
		if !isValidActivityRecord(record) {
			continue
		}
		overlap := activityInterval{
			start: maxTime(record.StartedAt, start),
			end:   minTime(record.EndedAt, end),
		}
		if !overlap.start.Before(overlap.end) {
			continue
		}

		duration := overlap.end.Sub(overlap.start)
		if isFirefoxDesktopRecord(record) {
			duration = d.coverage[record.DeviceID].visibleUncoveredDuration(
				overlap,
				minEffectiveSliceDuration,
			)
		}
		if duration > 0 {
			durations[record.AppName] += duration
		}
	}

	totals := make([]AppTotalRow, 0, len(durations))
	for appName, duration := range durations {
		totals = append(totals, AppTotalRow{
			AppName: appName,
			Seconds: int64(math.Round(duration.Seconds())),
		})
	}
	slices.SortFunc(totals, func(a, b AppTotalRow) int {
		if order := cmp.Compare(b.Seconds, a.Seconds); order != 0 {
			return order
		}
		return cmp.Compare(a.AppName, b.AppName)
	})
	return totals
}

func (c activityCoverage) visibleUncoveredDuration(
	subject activityInterval,
	minimum time.Duration,
) time.Duration {
	left := sort.Search(len(c.intervals), func(i int) bool {
		return c.intervals[i].end.After(subject.start)
	})
	right := sort.Search(len(c.intervals), func(i int) bool {
		return !c.intervals[i].start.Before(subject.end)
	})
	if left >= right {
		return durationAtLeast(subject.end.Sub(subject.start), minimum)
	}

	duration := durationAtLeast(
		minTime(c.intervals[left].start, subject.end).Sub(subject.start),
		minimum,
	)
	if right-left > 1 {
		duration += c.visibleGapPrefix[right-1] - c.visibleGapPrefix[left]
	}
	duration += durationAtLeast(
		subject.end.Sub(maxTime(c.intervals[right-1].end, subject.start)),
		minimum,
	)
	return duration
}

func durationAtLeast(duration, minimum time.Duration) time.Duration {
	if duration < minimum {
		return 0
	}
	return duration
}

func visitUncoveredActivityIntervals(
	subject activityInterval,
	coverage []activityInterval,
	work *int,
	workLimit int,
	visit func(activityInterval),
) bool {
	cursor := subject.start
	index := sort.Search(len(coverage), func(i int) bool {
		return coverage[i].end.After(cursor)
	})
	for ; index < len(coverage); index++ {
		interval := coverage[index]
		if !interval.start.Before(subject.end) {
			break
		}
		if *work >= workLimit {
			return false
		}
		(*work)++

		if interval.start.After(cursor) {
			visit(activityInterval{start: cursor, end: minTime(interval.start, subject.end)})
		}
		if interval.end.After(cursor) {
			cursor = interval.end
		}
		if !cursor.Before(subject.end) {
			return true
		}
	}
	if cursor.Before(subject.end) {
		visit(activityInterval{start: cursor, end: subject.end})
	}
	return true
}

func isValidActivityRecord(record *ActivityRecordRow) bool {
	return record.StartedAt.Before(record.EndedAt)
}

func isFirefoxBrowserRecord(record *ActivityRecordRow) bool {
	return icon.AppKey(record.AppName) == firefoxAppKey && hasActivityURL(record)
}

func isFirefoxDesktopRecord(record *ActivityRecordRow) bool {
	return icon.AppKey(record.AppName) == firefoxAppKey && !hasActivityURL(record)
}

func hasActivityURL(record *ActivityRecordRow) bool {
	return record.URL != nil && strings.TrimSpace(*record.URL) != ""
}

func mergeActivityIntervals(intervals []activityInterval) []activityInterval {
	if len(intervals) == 0 {
		return nil
	}

	slices.SortFunc(intervals, func(a, b activityInterval) int {
		if order := a.start.Compare(b.start); order != 0 {
			return order
		}
		return a.end.Compare(b.end)
	})

	merged := make([]activityInterval, 0, len(intervals))
	for _, interval := range intervals {
		if len(merged) == 0 || interval.start.After(merged[len(merged)-1].end) {
			merged = append(merged, interval)
			continue
		}
		if interval.end.After(merged[len(merged)-1].end) {
			merged[len(merged)-1].end = interval.end
		}
	}
	return merged
}

func compareActivityRecords(a, b *ActivityRecordRow) int {
	if order := a.StartedAt.Compare(b.StartedAt); order != 0 {
		return order
	}
	if order := cmp.Compare(a.DeviceID, b.DeviceID); order != 0 {
		return order
	}
	return cmp.Compare(a.ID, b.ID)
}

type activityRecordMaxHeap []*ActivityRecordRow

func (h *activityRecordMaxHeap) Len() int { return len(*h) }
func (h *activityRecordMaxHeap) Less(i, j int) bool {
	return compareActivityRecords((*h)[i], (*h)[j]) > 0
}
func (h *activityRecordMaxHeap) Swap(i, j int) { (*h)[i], (*h)[j] = (*h)[j], (*h)[i] }
func (h *activityRecordMaxHeap) Push(value any) {
	record, ok := value.(*ActivityRecordRow)
	if !ok {
		panic("activity record heap received an unexpected value")
	}
	*h = append(*h, record)
}

func (h *activityRecordMaxHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

type boundedActivityRecords struct {
	limit     int
	records   activityRecordMaxHeap
	truncated bool
}

func newBoundedActivityRecords(limit int) *boundedActivityRecords {
	return &boundedActivityRecords{
		limit:   max(0, limit),
		records: make(activityRecordMaxHeap, 0, max(0, limit)),
	}
}

func (b *boundedActivityRecords) add(record *ActivityRecordRow) {
	if b.limit == 0 {
		b.truncated = true
		return
	}
	if len(b.records) < b.limit {
		heap.Push(&b.records, record)
		return
	}

	b.truncated = true
	if compareActivityRecords(record, b.records[0]) < 0 {
		b.records[0] = record
		heap.Fix(&b.records, 0)
	}
}

func (b *boundedActivityRecords) sorted() []ActivityRecordRow {
	pointers := slices.Clone(b.records)
	slices.SortFunc(pointers, compareActivityRecords)
	records := make([]ActivityRecordRow, len(pointers))
	for i := range pointers {
		records[i] = *pointers[i]
	}
	return records
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
