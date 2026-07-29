package db

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/yelinaung/trackkr/internal/icon"
)

const firefoxAppKey = "firefox"

type activityInterval struct {
	start time.Time
	end   time.Time
}

// deduplicateFirefoxActivity gives URL-bearing Firefox observations precedence
// over desktop Firefox observations for the same device and time interval.
func deduplicateFirefoxActivity(records []ActivityRecordRow) []ActivityRecordRow {
	coverage := make(map[int64][]activityInterval)
	for i := range records {
		record := &records[i]
		if isFirefoxBrowserRecord(record) && record.StartedAt.Before(record.EndedAt) {
			coverage[record.DeviceID] = append(coverage[record.DeviceID], activityInterval{
				start: record.StartedAt,
				end:   record.EndedAt,
			})
		}
	}
	for deviceID, intervals := range coverage {
		coverage[deviceID] = mergeActivityIntervals(intervals)
	}

	effective := make([]ActivityRecordRow, 0, len(records))
	for i := range records {
		record := &records[i]
		if !isFirefoxDesktopRecord(record) || !record.StartedAt.Before(record.EndedAt) {
			effective = append(effective, *record)
			continue
		}

		for _, interval := range subtractActivityIntervals(
			activityInterval{start: record.StartedAt, end: record.EndedAt},
			coverage[record.DeviceID],
		) {
			slice := *record
			slice.StartedAt = interval.start
			slice.EndedAt = interval.end
			slice.DurationS = int(interval.end.Sub(interval.start).Seconds())
			effective = append(effective, slice)
		}
	}

	slices.SortFunc(effective, func(a, b ActivityRecordRow) int {
		if order := a.StartedAt.Compare(b.StartedAt); order != 0 {
			return order
		}
		if order := cmp.Compare(a.DeviceID, b.DeviceID); order != 0 {
			return order
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return effective
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

func subtractActivityIntervals(subject activityInterval, coverage []activityInterval) []activityInterval {
	cursor := subject.start
	remaining := make([]activityInterval, 0, 2)
	for _, interval := range coverage {
		if !interval.end.After(cursor) {
			continue
		}
		if !interval.start.Before(subject.end) {
			break
		}
		if interval.start.After(cursor) {
			remaining = append(remaining, activityInterval{
				start: cursor,
				end:   minTime(interval.start, subject.end),
			})
		}
		if interval.end.After(cursor) {
			cursor = interval.end
		}
		if !cursor.Before(subject.end) {
			return remaining
		}
	}
	if cursor.Before(subject.end) {
		remaining = append(remaining, activityInterval{start: cursor, end: subject.end})
	}
	return remaining
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func appTotals(records []ActivityRecordRow, start, end time.Time) []AppTotalRow {
	durations := make(map[string]time.Duration)
	for i := range records {
		record := &records[i]
		overlapStart := record.StartedAt
		if overlapStart.Before(start) {
			overlapStart = start
		}
		overlapEnd := record.EndedAt
		if overlapEnd.After(end) {
			overlapEnd = end
		}
		if overlapStart.Before(overlapEnd) {
			durations[record.AppName] += overlapEnd.Sub(overlapStart)
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
