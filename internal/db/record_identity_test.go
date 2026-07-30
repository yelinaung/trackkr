package db

import (
	"context"
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/identity"
)

const testChromeApp = "Google Chrome"

// The pair (device_id, started_at) could not tell these four apart. Losing any
// of them to ON CONFLICT DO NOTHING looked exactly like an accepted write.
func TestSameStartDistinctRecordsAllInsert(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	device := newActivityDevice(t, pool, q)

	start := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Minute)
	url := "https://example.com/"

	rows := []ActivityRecordRow{
		{DeviceID: device.ID, Producer: identity.ProducerDesktop, AppName: "Firefox", StartedAt: start, EndedAt: end, DurationS: 60},
		{DeviceID: device.ID, Producer: identity.ProducerFirefox, AppName: "Firefox", URL: &url, StartedAt: start, EndedAt: end, DurationS: 60},
		{DeviceID: device.ID, Producer: identity.ProducerChrome, AppName: testChromeApp, URL: &url, StartedAt: start, EndedAt: end, DurationS: 60},
		// A second window of the same browser, active at the same instant.
		{DeviceID: device.ID, Producer: identity.ProducerChrome, AppName: testChromeApp, URL: &url, StartedAt: start, EndedAt: end, DurationS: 60},
	}

	accepted, err := q.InsertActivityRecords(ctx, stampIdentity(t, rows))
	if err != nil {
		t.Fatalf("InsertActivityRecords: %v", err)
	}
	if accepted != 4 {
		t.Errorf("accepted = %d, want all 4 distinct identities", accepted)
	}
}

// A reporter retry preserves the record ID, so the replay must insert nothing.
func TestReplayingARecordIDInsertsNothing(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	device := newActivityDevice(t, pool, q)

	start := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	recordID, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	row := ActivityRecordRow{
		DeviceID:  device.ID,
		RecordID:  recordID,
		Producer:  identity.ProducerChrome,
		AppName:   testChromeApp,
		StartedAt: start,
		EndedAt:   start.Add(time.Minute),
		DurationS: 60,
	}

	first, err := q.InsertActivityRecords(ctx, []ActivityRecordRow{row})
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if first != 1 {
		t.Fatalf("first insert accepted = %d, want 1", first)
	}

	// The same ID with different content is still the same logical segment.
	row.Title = "changed on retry"
	row.EndedAt = start.Add(2 * time.Minute)
	replay, err := q.InsertActivityRecords(ctx, []ActivityRecordRow{row})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay != 0 {
		t.Errorf("replay accepted = %d, want 0", replay)
	}
}

// The same record ID on two devices is two records: the identity is scoped.
func TestRecordIDIsScopedToItsDevice(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	device := newActivityDevice(t, pool, q)

	second, err := q.CreateDevice(ctx, device.UserID, "second", "desktop", "second-key-"+device.APIKey)
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	recordID, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 7, 30, 11, 0, 0, 0, time.UTC)
	rows := []ActivityRecordRow{
		{DeviceID: device.ID, RecordID: recordID, Producer: identity.ProducerDesktop, AppName: "Ghostty", StartedAt: start, EndedAt: start.Add(time.Minute), DurationS: 60},
		{DeviceID: second.ID, RecordID: recordID, Producer: identity.ProducerDesktop, AppName: "Ghostty", StartedAt: start, EndedAt: start.Add(time.Minute), DurationS: 60},
	}

	accepted, err := q.InsertActivityRecords(ctx, rows)
	if err != nil {
		t.Fatalf("InsertActivityRecords: %v", err)
	}
	if accepted != 2 {
		t.Errorf("accepted = %d, want 2 across devices", accepted)
	}
}

// The producer column is trusted to scope deduplication, so the database
// refuses anything outside the enum.
func TestProducerEnumIsEnforced(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	device := newActivityDevice(t, pool, q)

	recordID, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	_, err = q.InsertActivityRecords(ctx, []ActivityRecordRow{{
		DeviceID:  device.ID,
		RecordID:  recordID,
		Producer:  "safari",
		AppName:   "Safari",
		StartedAt: start,
		EndedAt:   start.Add(time.Minute),
		DurationS: 60,
	}})
	if err == nil {
		t.Error("an unknown producer was accepted")
	}
}

// The backfill classified URL-bearing Firefox rows as the firefox producer and
// everything else as desktop. Rows written since then carry their own.
func TestMigratedRowsCarryAProducer(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	var bad int
	if err := pool.QueryRow(
		ctx,
		`SELECT count(*) FROM activity_records
		 WHERE producer NOT IN ('desktop', 'firefox', 'chrome') OR record_id IS NULL`,
	).Scan(&bad); err != nil {
		t.Fatalf("counting migrated rows: %v", err)
	}
	if bad != 0 {
		t.Errorf("%d rows lack a valid identity after migration 006", bad)
	}
}
