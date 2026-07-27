package db

import (
	"context"
	"fmt"
	"testing"
	"time"
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
