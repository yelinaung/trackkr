package db

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	testFirefoxApp = "Firefox"
	testPageTitle  = "Test Page"
)

func TestCreateUser(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	username := fmt.Sprintf("testuser_%d", time.Now().UnixNano())
	user, err := q.CreateUser(ctx, username, "$2a$10$fakehash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupUser(t, pool, user.ID) })

	if user.ID == 0 {
		t.Error("expected non-zero ID")
	}
	if user.Username != username {
		t.Errorf("username = %q, want %q", user.Username, username)
	}
}

func TestCreateUserDuplicate(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	username := fmt.Sprintf("testuser_%d", time.Now().UnixNano())
	user, err := q.CreateUser(ctx, username, "$2a$10$fakehash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupUser(t, pool, user.ID) })

	_, err = q.CreateUser(ctx, username, "$2a$10$otherhash")
	if err == nil {
		t.Error("expected error for duplicate username")
	}
}

func TestGetUserByUsername(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	username := fmt.Sprintf("testuser_%d", time.Now().UnixNano())
	created, err := q.CreateUser(ctx, username, "$2a$10$fakehash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupUser(t, pool, created.ID) })

	found, err := q.GetUserByUsername(ctx, username)
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if found.ID != created.ID {
		t.Errorf("ID = %d, want %d", found.ID, created.ID)
	}
}

func TestGetUserByUsernameNotFound(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	_, err := q.GetUserByUsername(ctx, "nonexistent_user_xyz")
	if err == nil {
		t.Error("expected error for nonexistent user")
	}
}

func TestCreateDevice(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	username := fmt.Sprintf("testuser_%d", time.Now().UnixNano())
	user, err := q.CreateUser(ctx, username, "$2a$10$fakehash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupUser(t, pool, user.ID) })

	apiKey := fmt.Sprintf("testkey_%d", time.Now().UnixNano())
	device, err := q.CreateDevice(ctx, user.ID, "test-laptop", "desktop", apiKey)
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	if device.Name != "test-laptop" {
		t.Errorf("Name = %q, want %q", device.Name, "test-laptop")
	}
	if device.DeviceType != "desktop" {
		t.Errorf("DeviceType = %q, want %q", device.DeviceType, "desktop")
	}
	if device.APIKey != apiKey {
		t.Errorf("APIKey = %q, want %q", device.APIKey, apiKey)
	}
}

func TestGetDeviceByAPIKey(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	username := fmt.Sprintf("testuser_%d", time.Now().UnixNano())
	user, err := q.CreateUser(ctx, username, "$2a$10$fakehash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupUser(t, pool, user.ID) })

	apiKey := fmt.Sprintf("testkey_%d", time.Now().UnixNano())
	created, err := q.CreateDevice(ctx, user.ID, "test-laptop", "desktop", apiKey)
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	found, err := q.GetDeviceByAPIKey(ctx, apiKey)
	if err != nil {
		t.Fatalf("GetDeviceByAPIKey: %v", err)
	}
	if found.ID != created.ID {
		t.Errorf("ID = %d, want %d", found.ID, created.ID)
	}
}

func TestGetDeviceByAPIKeyNotFound(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	_, err := q.GetDeviceByAPIKey(ctx, "nonexistent_key_xyz")
	if err == nil {
		t.Error("expected error for nonexistent API key")
	}
}

func TestListDevicesByUser(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	username := fmt.Sprintf("testuser_%d", time.Now().UnixNano())
	user, err := q.CreateUser(ctx, username, "$2a$10$fakehash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupUser(t, pool, user.ID) })

	ts := time.Now().UnixNano()
	for i := range 3 {
		apiKey := fmt.Sprintf("testkey_%d_%d", ts, i)
		name := fmt.Sprintf("device-%d", i)
		_, err := q.CreateDevice(ctx, user.ID, name, "desktop", apiKey)
		if err != nil {
			t.Fatalf("CreateDevice[%d]: %v", i, err)
		}
	}

	devices, err := q.ListDevicesByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListDevicesByUser: %v", err)
	}
	if len(devices) != 3 {
		t.Errorf("got %d devices, want 3", len(devices))
	}
}

func TestDeleteDevice(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	username := fmt.Sprintf("testuser_%d", time.Now().UnixNano())
	user, err := q.CreateUser(ctx, username, "$2a$10$fakehash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupUser(t, pool, user.ID) })

	apiKey := fmt.Sprintf("testkey_%d", time.Now().UnixNano())
	device, err := q.CreateDevice(ctx, user.ID, "test-laptop", "desktop", apiKey)
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	if err := q.DeleteDevice(ctx, device.ID, user.ID); err != nil {
		t.Fatalf("DeleteDevice: %v", err)
	}

	devices, err := q.ListDevicesByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListDevicesByUser: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("got %d devices after delete, want 0", len(devices))
	}
}

func TestInsertActivityRecords(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	username := fmt.Sprintf("testuser_%d", time.Now().UnixNano())
	user, err := q.CreateUser(ctx, username, "$2a$10$fakehash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupUser(t, pool, user.ID) })

	apiKey := fmt.Sprintf("testkey_%d", time.Now().UnixNano())
	device, err := q.CreateDevice(ctx, user.ID, "test-laptop", "desktop", apiKey)
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	now := time.Now().Truncate(time.Second)
	records := []ActivityRecordRow{
		{
			DeviceID:  device.ID,
			AppName:   testFirefoxApp,
			Title:     testPageTitle,
			StartedAt: now,
			EndedAt:   now.Add(30 * time.Second),
			DurationS: 30,
		},
		{
			DeviceID:  device.ID,
			AppName:   "VS Code",
			Title:     "main.go",
			StartedAt: now.Add(30 * time.Second),
			EndedAt:   now.Add(60 * time.Second),
			DurationS: 30,
		},
	}

	accepted, err := q.InsertActivityRecords(ctx, records)
	if err != nil {
		t.Fatalf("InsertActivityRecords: %v", err)
	}
	if accepted != 2 {
		t.Errorf("accepted = %d, want 2", accepted)
	}
}

func TestInsertActivityRecordsDuplicates(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	username := fmt.Sprintf("testuser_%d", time.Now().UnixNano())
	user, err := q.CreateUser(ctx, username, "$2a$10$fakehash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupUser(t, pool, user.ID) })

	apiKey := fmt.Sprintf("testkey_%d", time.Now().UnixNano())
	device, err := q.CreateDevice(ctx, user.ID, "test-laptop", "desktop", apiKey)
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	now := time.Now().Truncate(time.Second)
	records := []ActivityRecordRow{
		{
			DeviceID:  device.ID,
			AppName:   testFirefoxApp,
			Title:     testPageTitle,
			StartedAt: now,
			EndedAt:   now.Add(30 * time.Second),
			DurationS: 30,
		},
	}

	_, err = q.InsertActivityRecords(ctx, records)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Same record again — ON CONFLICT DO NOTHING should deduplicate
	accepted, err := q.InsertActivityRecords(ctx, records)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if accepted != 0 {
		t.Errorf("accepted = %d on duplicate, want 0", accepted)
	}
}

func TestGetActivityRecords(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	username := fmt.Sprintf("testuser_%d", time.Now().UnixNano())
	user, err := q.CreateUser(ctx, username, "$2a$10$fakehash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupUser(t, pool, user.ID) })

	apiKey := fmt.Sprintf("testkey_%d", time.Now().UnixNano())
	device, err := q.CreateDevice(ctx, user.ID, "test-laptop", "desktop", apiKey)
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	now := time.Now().Truncate(time.Second)
	url := "https://example.com"
	records := []ActivityRecordRow{
		{
			DeviceID:  device.ID,
			AppName:   testFirefoxApp,
			Title:     testPageTitle,
			URL:       &url,
			StartedAt: now,
			EndedAt:   now.Add(30 * time.Second),
			DurationS: 30,
		},
		{
			DeviceID:  device.ID,
			AppName:   "VS Code",
			Title:     "main.go",
			StartedAt: now.Add(30 * time.Second),
			EndedAt:   now.Add(60 * time.Second),
			DurationS: 30,
		},
	}

	if _, err := q.InsertActivityRecords(ctx, records); err != nil {
		t.Fatalf("InsertActivityRecords: %v", err)
	}

	// All records in range
	got, err := q.GetActivityRecords(ctx, user.ID, now.Add(-time.Minute), now.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("GetActivityRecords: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d records, want 2", len(got))
	}

	// With device filter
	deviceID := device.ID
	got, err = q.GetActivityRecords(ctx, user.ID, now.Add(-time.Minute), now.Add(time.Minute), &deviceID)
	if err != nil {
		t.Fatalf("GetActivityRecords with device filter: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d records with device filter, want 2", len(got))
	}

	// Time range that excludes records
	got, err = q.GetActivityRecords(ctx, user.ID, now.Add(time.Hour), now.Add(2*time.Hour), nil)
	if err != nil {
		t.Fatalf("GetActivityRecords empty range: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d records for empty range, want 0", len(got))
	}

	// Verify URL was stored
	got, err = q.GetActivityRecords(ctx, user.ID, now.Add(-time.Minute), now.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("GetActivityRecords for URL check: %v", err)
	}
	if got[0].URL == nil || *got[0].URL != url {
		t.Errorf("URL = %v, want %q", got[0].URL, url)
	}
	if got[1].URL != nil {
		t.Errorf("URL = %v, want nil", got[1].URL)
	}
}

func TestGetUserByID(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	username := fmt.Sprintf("testuser_%d", time.Now().UnixNano())
	created, err := q.CreateUser(ctx, username, "$2a$10$fakehash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupUser(t, pool, created.ID) })

	got, err := q.GetUserByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got.Username != username {
		t.Errorf("username = %q, want %q", got.Username, username)
	}

	if _, err := q.GetUserByID(ctx, -1); err == nil {
		t.Error("expected an error for a missing user")
	}
}

// A record spanning midnight must appear on both days, clamped by the
// caller. Selecting on started_at alone would hide it on the later day.
func TestGetActivityRecordsSelectsOverlap(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	user, device := seedUserAndDevice(t, pool, q)

	day := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC)
	prevEvening := day.Add(-10 * time.Minute)
	insertRecord(t, q, device.ID, "firefox", prevEvening, day.Add(20*time.Minute))

	for _, tc := range []struct {
		name  string
		start time.Time
	}{
		{"day the record started", day.AddDate(0, 0, -1)},
		{"day the record ended", day},
	} {
		records, err := q.GetActivityRecords(ctx, user.ID, tc.start, tc.start.AddDate(0, 0, 1), nil)
		if err != nil {
			t.Fatalf("%s: GetActivityRecords: %v", tc.name, err)
		}
		if len(records) != 1 {
			t.Errorf("%s: records = %d, want 1", tc.name, len(records))
		}
	}
}

// Summing duration_s over overlapping rows would count a boundary record
// in full on both days; only the overlapping slice belongs to each.
func TestGetAppTotalsCountsOnlyOverlap(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	user, device := seedUserAndDevice(t, pool, q)

	day := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// 10 minutes before midnight, 20 minutes after.
	insertRecord(t, q, device.ID, testFirefoxApp, day.Add(-10*time.Minute), day.Add(20*time.Minute))

	totals, err := q.GetAppTotals(ctx, user.ID, day, day.AddDate(0, 0, 1), nil)
	if err != nil {
		t.Fatalf("GetAppTotals: %v", err)
	}
	if len(totals) != 1 {
		t.Fatalf("totals = %+v, want one app", totals)
	}
	if totals[0].Seconds != 1200 {
		t.Errorf("seconds = %d, want 1200 (only the part inside the day)", totals[0].Seconds)
	}

	prev, err := q.GetAppTotals(ctx, user.ID, day.AddDate(0, 0, -1), day, nil)
	if err != nil {
		t.Fatalf("GetAppTotals (previous day): %v", err)
	}
	if len(prev) != 1 || prev[0].Seconds != 600 {
		t.Errorf("previous day = %+v, want 600 seconds", prev)
	}
}

// Migration 002 adds ON DELETE CASCADE; without it this fails with a
// foreign key violation, which is exactly the bug the migration fixes.
func TestDeleteDeviceCascadesRecords(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	user, device := seedUserAndDevice(t, pool, q)

	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	insertRecord(t, q, device.ID, testFirefoxApp, start, start.Add(time.Hour))

	if err := q.DeleteDevice(ctx, device.ID, user.ID); err != nil {
		t.Fatalf("DeleteDevice with records: %v", err)
	}

	var remaining int
	err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM activity_records WHERE device_id = $1`, device.ID).Scan(&remaining)
	if err != nil {
		t.Fatalf("counting records: %v", err)
	}
	if remaining != 0 {
		t.Errorf("records left after cascade = %d, want 0", remaining)
	}
}

func seedUserAndDevice(t *testing.T, pool *pgxpool.Pool, q *Queries) (*UserRow, *DeviceRow) {
	t.Helper()
	ctx := context.Background()

	username := fmt.Sprintf("testuser_%d", time.Now().UnixNano())
	user, err := q.CreateUser(ctx, username, "$2a$10$fakehash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupUser(t, pool, user.ID) })

	apiKey := fmt.Sprintf("key_%d", time.Now().UnixNano())
	device, err := q.CreateDevice(ctx, user.ID, "test-laptop", "desktop", apiKey)
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	return user, device
}

func insertRecord(t *testing.T, q *Queries, deviceID int64, app string, start, end time.Time) {
	t.Helper()

	_, err := q.InsertActivityRecords(context.Background(), []ActivityRecordRow{{
		DeviceID:  deviceID,
		AppName:   app,
		StartedAt: start,
		EndedAt:   end,
		DurationS: int(end.Sub(start).Seconds()),
	}})
	if err != nil {
		t.Fatalf("InsertActivityRecords: %v", err)
	}
}

// The dashboard groups desktop activity by app, which collapses every
// browser record into one "Firefox" row. Sites answer the question that
// actually matters for browsing: which pages the time went to.
func TestGetSiteTotals(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	user, device := seedUserAndDevice(t, pool, q)
	day := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)

	insertRecordWithURL(t, q, device.ID, "https://www.youtube.com/watch?v=abc",
		day.Add(10*time.Hour), day.Add(10*time.Hour+30*time.Minute))
	insertRecordWithURL(t, q, device.ID, "https://youtube.com/watch?v=def",
		day.Add(11*time.Hour), day.Add(11*time.Hour+10*time.Minute))
	insertRecordWithURL(t, q, device.ID, "https://github.com/yelinaung/trackkr",
		day.Add(12*time.Hour), day.Add(12*time.Hour+5*time.Minute))
	// A desktop record has no URL and must not appear.
	insertRecord(t, q, device.ID, "ghostty", day.Add(13*time.Hour), day.Add(14*time.Hour))

	totals, err := q.GetSiteTotals(ctx, user.ID, day, day.AddDate(0, 0, 1), nil)
	if err != nil {
		t.Fatalf("GetSiteTotals: %v", err)
	}
	if len(totals) != 2 {
		t.Fatalf("sites = %+v, want youtube.com and github.com only", totals)
	}

	// www. is stripped, so one site is one row rather than two.
	if totals[0].Site != "youtube.com" || totals[0].Seconds != 2400 {
		t.Errorf("first = %+v, want youtube.com with 2400s", totals[0])
	}
	if totals[1].Site != "github.com" || totals[1].Seconds != 300 {
		t.Errorf("second = %+v, want github.com with 300s", totals[1])
	}
}

// The authority is not the hostname: it also carries userinfo and a
// port. Grouping on it would print credentials on the dashboard and
// split one site across several rows.
func TestGetSiteTotalsUsesHostnameOnly(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	user, device := seedUserAndDevice(t, pool, q)
	day := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)

	// All seven are the same site once the hostname is extracted.
	for i, url := range []string{
		"https://example.com/a",
		"https://www.example.com/b",
		"https://EXAMPLE.com/c",
		"https://example.com:8443/d",
		"https://someone:hunter2@example.com/e",
		"https://example.com./f",
		"https://www.example.com./g",
	} {
		start := day.Add(time.Duration(i) * time.Hour)
		insertRecordWithURL(t, q, device.ID, url, start, start.Add(time.Minute))
	}

	totals, err := q.GetSiteTotals(ctx, user.ID, day, day.AddDate(0, 0, 1), nil)
	if err != nil {
		t.Fatalf("GetSiteTotals: %v", err)
	}

	if len(totals) != 1 {
		t.Fatalf("sites = %+v, want them all grouped as example.com", totals)
	}
	if totals[0].Site != "example.com" {
		t.Errorf("site = %q, want example.com", totals[0].Site)
	}
	if totals[0].Seconds != 420 {
		t.Errorf("seconds = %d, want 420 across the seven visits", totals[0].Seconds)
	}
	// The password must not reach the dashboard by any route.
	if strings.Contains(totals[0].Site, "hunter2") || strings.Contains(totals[0].Site, "@") {
		t.Errorf("site %q leaked URL credentials", totals[0].Site)
	}
}

// A bracketed IPv6 literal must survive port stripping intact.
func TestGetSiteTotalsKeepsIPv6Literals(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	user, device := seedUserAndDevice(t, pool, q)
	day := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)

	insertRecordWithURL(t, q, device.ID, "http://[::1]:7600/extension/status",
		day.Add(9*time.Hour), day.Add(9*time.Hour+time.Minute))
	insertRecordWithURL(t, q, device.ID, "http://127.0.0.1:8080/devices",
		day.Add(10*time.Hour), day.Add(10*time.Hour+time.Minute))

	totals, err := q.GetSiteTotals(ctx, user.ID, day, day.AddDate(0, 0, 1), nil)
	if err != nil {
		t.Fatalf("GetSiteTotals: %v", err)
	}

	got := make(map[string]bool, len(totals))
	for _, row := range totals {
		got[row.Site] = true
	}
	for _, want := range []string{"[::1]", "127.0.0.1"} {
		if !got[want] {
			t.Errorf("missing %q; got %+v", want, totals)
		}
	}
}

// A record spanning midnight must contribute only its share, as the app
// totals do.
func TestGetSiteTotalsCountsOnlyOverlap(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := context.Background()

	user, device := seedUserAndDevice(t, pool, q)
	day := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	insertRecordWithURL(t, q, device.ID, "https://example.com/late",
		day.Add(-10*time.Minute), day.Add(20*time.Minute))

	totals, err := q.GetSiteTotals(ctx, user.ID, day, day.AddDate(0, 0, 1), nil)
	if err != nil {
		t.Fatalf("GetSiteTotals: %v", err)
	}
	if len(totals) != 1 || totals[0].Seconds != 1200 {
		t.Errorf("totals = %+v, want 1200s inside the day", totals)
	}
}

func insertRecordWithURL(t *testing.T, q *Queries, deviceID int64, url string, start, end time.Time) {
	t.Helper()

	_, err := q.InsertActivityRecords(context.Background(), []ActivityRecordRow{{
		DeviceID:  deviceID,
		AppName:   testFirefoxApp,
		Title:     "a page",
		URL:       &url,
		StartedAt: start,
		EndedAt:   end,
		DurationS: int(end.Sub(start).Seconds()),
	}})
	if err != nil {
		t.Fatalf("InsertActivityRecords: %v", err)
	}
}
