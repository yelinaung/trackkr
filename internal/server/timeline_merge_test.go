package server

import (
	"slices"
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/db"
	"hegel.dev/go/hegel"
)

const testGhosttyApp = "Ghostty"

// coveredMinutes totals the union of a run's intervals, per device and
// application, as a naive sweep over sorted copies.
func coveredMinutes(records []db.ActivityRecordRow) map[string]time.Duration {
	type lane struct {
		device int64
		app    string
	}
	byLane := make(map[lane][]db.ActivityRecordRow)
	for i := range records {
		key := lane{records[i].DeviceID, records[i].AppName}
		byLane[key] = append(byLane[key], records[i])
	}

	covered := make(map[string]time.Duration)
	for key, group := range byLane {
		slices.SortFunc(group, func(a, b db.ActivityRecordRow) int {
			return a.StartedAt.Compare(b.StartedAt)
		})
		var total time.Duration
		var cursor time.Time
		for i := range group {
			from, to := group[i].StartedAt, group[i].EndedAt
			if !to.After(from) {
				continue
			}
			if from.Before(cursor) {
				from = cursor
			}
			if to.After(from) {
				total += to.Sub(from)
				cursor = to
			}
		}
		covered[key.app] += total
	}
	return covered
}

// TestMergeAdjacentActivityConservesCoveredTime is the general form of
// TestMergeAdjacentActivityPreservesCoveredTime: merging is honest only
// because a merged bar spans exactly what the records it replaced did.
//
// The property also holds merging to its limit -- a pause belongs on the
// chart, so a merged bar may never bridge a gap between two records.
func TestMergeAdjacentActivityConservesCoveredTime(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		base := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
		records := drawDayRecords(ht, base)
		// mergeAdjacentActivity documents that records arrive sorted by
		// (StartedAt, DeviceID, ID), and the previous bar for a device
		// is its only merge candidate.
		slices.SortFunc(records, func(a, b db.ActivityRecordRow) int {
			if order := a.StartedAt.Compare(b.StartedAt); order != 0 {
				return order
			}
			return int(a.DeviceID - b.DeviceID)
		})

		merged := mergeAdjacentActivity(records)
		if len(merged) > len(records) {
			ht.Fatalf("merging %d records produced %d", len(records), len(merged))
		}

		before, after := coveredMinutes(records), coveredMinutes(merged)
		for app, want := range before {
			if after[app] != want {
				ht.Fatalf("application %q covered %s before merging and %s after",
					app, want, after[app])
			}
		}
		for app, got := range after {
			if before[app] != got {
				ht.Fatalf("application %q covered %s after merging and %s before",
					app, got, before[app])
			}
		}

		for i := range merged {
			if merged[i].EndedAt.Before(merged[i].StartedAt) {
				ht.Fatalf("merged bar %d runs backwards", i)
			}
			if want := int(merged[i].EndedAt.Sub(merged[i].StartedAt).Seconds()); merged[i].DurationS != want {
				ht.Fatalf("merged bar %d reports %ds over a %ds span",
					i, merged[i].DurationS, want)
			}
		}
	})
}

func mergeRecord(id, device int64, app, title string, from, to time.Time) db.ActivityRecordRow {
	return db.ActivityRecordRow{
		ID:        id,
		DeviceID:  device,
		AppName:   app,
		Title:     title,
		StartedAt: from,
		EndedAt:   to,
		DurationS: int(to.Sub(from).Seconds()),
	}
}

func TestMergeAdjacentActivity(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }

	tests := []struct {
		name    string
		records []db.ActivityRecordRow
		want    []struct {
			app      string
			from, to int
		}
	}{
		{
			name: "a run of touching records becomes one bar",
			records: []db.ActivityRecordRow{
				mergeRecord(1, 1, testFirefoxApp, "a", at(0), at(1)),
				mergeRecord(2, 1, testFirefoxApp, "b", at(1), at(2)),
				mergeRecord(3, 1, testFirefoxApp, "c", at(2), at(9)),
			},
			want: []struct {
				app      string
				from, to int
			}{{testFirefoxApp, 0, 9}},
		},
		{
			name: "a gap is preserved",
			records: []db.ActivityRecordRow{
				mergeRecord(1, 1, testFirefoxApp, "a", at(0), at(2)),
				mergeRecord(2, 1, testFirefoxApp, "b", at(5), at(7)),
			},
			want: []struct {
				app      string
				from, to int
			}{{testFirefoxApp, 0, 2}, {testFirefoxApp, 5, 7}},
		},
		{
			name: "a different application breaks the run",
			records: []db.ActivityRecordRow{
				mergeRecord(1, 1, testFirefoxApp, "a", at(0), at(2)),
				mergeRecord(2, 1, testGhosttyApp, "b", at(2), at(3)),
				mergeRecord(3, 1, testFirefoxApp, "c", at(3), at(6)),
			},
			want: []struct {
				app      string
				from, to int
			}{{testFirefoxApp, 0, 2}, {testGhosttyApp, 2, 3}, {testFirefoxApp, 3, 6}},
		},
		{
			name: "devices never merge into each other",
			records: []db.ActivityRecordRow{
				mergeRecord(1, 1, testFirefoxApp, "a", at(0), at(2)),
				mergeRecord(2, 2, testFirefoxApp, "b", at(2), at(4)),
				mergeRecord(3, 1, testFirefoxApp, "c", at(2), at(6)),
			},
			want: []struct {
				app      string
				from, to int
			}{{testFirefoxApp, 0, 6}, {testFirefoxApp, 2, 4}},
		},
		{
			name: "an enclosed record does not shorten the run",
			records: []db.ActivityRecordRow{
				mergeRecord(1, 1, testFirefoxApp, "a", at(0), at(9)),
				mergeRecord(2, 1, testFirefoxApp, "b", at(2), at(3)),
			},
			want: []struct {
				app      string
				from, to int
			}{{testFirefoxApp, 0, 9}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mergeAdjacentActivity(tt.records)
			if len(got) != len(tt.want) {
				t.Fatalf("merged into %d records, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, want := range tt.want {
				if got[i].AppName != want.app {
					t.Errorf("record %d app = %q, want %q", i, got[i].AppName, want.app)
				}
				if !got[i].StartedAt.Equal(at(want.from)) || !got[i].EndedAt.Equal(at(want.to)) {
					t.Errorf("record %d spans %v..%v, want +%dm..+%dm",
						i, got[i].StartedAt, got[i].EndedAt, want.from, want.to)
				}
				if seconds := int(at(want.to).Sub(at(want.from)).Seconds()); got[i].DurationS != seconds {
					t.Errorf("record %d duration = %d, want %d", i, got[i].DurationS, seconds)
				}
			}
		})
	}
}

// Merging must not invent or lose time: a merged run covers exactly the union
// of the records it replaces.
func TestMergeAdjacentActivityPreservesCoveredTime(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	records := []db.ActivityRecordRow{
		mergeRecord(1, 1, testFirefoxApp, "a", base, base.Add(time.Minute)),
		mergeRecord(2, 1, testFirefoxApp, "b", base.Add(time.Minute), base.Add(4*time.Minute)),
		mergeRecord(3, 1, testGhosttyApp, "c", base.Add(4*time.Minute), base.Add(5*time.Minute)),
		mergeRecord(4, 1, testFirefoxApp, "d", base.Add(9*time.Minute), base.Add(11*time.Minute)),
	}

	var before time.Duration
	for _, record := range records {
		before += record.EndedAt.Sub(record.StartedAt)
	}
	var after time.Duration
	for _, record := range mergeAdjacentActivity(records) {
		after += record.EndedAt.Sub(record.StartedAt)
	}
	if before != after {
		t.Errorf("covered time = %v after merging, want %v", after, before)
	}
}

// A merged run spans several titles, so the tooltip must not claim one.
func TestMergeAdjacentActivityClearsMixedTitles(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	got := mergeAdjacentActivity([]db.ActivityRecordRow{
		mergeRecord(1, 1, testFirefoxApp, "Inbox", base, base.Add(time.Minute)),
		mergeRecord(2, 1, testFirefoxApp, "Timeline", base.Add(time.Minute), base.Add(2*time.Minute)),
	})
	if len(got) != 1 || got[0].Title != "" {
		t.Errorf("merged title = %q, want empty across differing titles", got[0].Title)
	}

	same := mergeAdjacentActivity([]db.ActivityRecordRow{
		mergeRecord(1, 1, testGhosttyApp, "tmux a", base, base.Add(time.Minute)),
		mergeRecord(2, 1, testGhosttyApp, "tmux a", base.Add(time.Minute), base.Add(2*time.Minute)),
	})
	if len(same) != 1 || same[0].Title != "tmux a" {
		t.Errorf("merged title = %q, want the shared title retained", same[0].Title)
	}
}
