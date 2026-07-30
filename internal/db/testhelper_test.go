package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/identity"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testSiteHost is the host every site-derivation fixture normalizes to.
const testSiteHost = "example.com"

func testDSN() string {
	if v := os.Getenv("TRACKKR_TEST_DSN"); v != "" {
		return v
	}
	return "postgres://trackkr:trackkr@localhost:5455/trackkr?sslmode=disable"
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testDSN()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("skipping: database not available: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("skipping: database not reachable: %v", err)
	}

	if err := RunMigrations(dsn); err != nil {
		pool.Close()
		t.Fatalf("running migrations: %v", err)
	}

	t.Cleanup(func() { pool.Close() })
	return pool
}

// cleanupUser deletes a test user and all associated data.
func cleanupUser(t *testing.T, pool *pgxpool.Pool, userID int64) {
	t.Helper()
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM activity_records WHERE device_id IN (SELECT id FROM devices WHERE user_id = $1)`, userID)
	_, _ = pool.Exec(ctx, `DELETE FROM devices WHERE user_id = $1`, userID)
	_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
}

// stampIdentity fills in the record identity production always supplies.
//
// Migration 006 makes record_id and producer NOT NULL, and the daemon mints
// them before anything reaches the database. These tests are about storage and
// query behaviour rather than identity, so they get a fresh UUID and a producer
// inferred the same way the legacy backfill infers one.
func stampIdentity(t *testing.T, rows []ActivityRecordRow) []ActivityRecordRow {
	t.Helper()
	for i := range rows {
		if rows[i].RecordID == "" {
			id, err := identity.New()
			if err != nil {
				t.Fatalf("minting a test record id: %v", err)
			}
			rows[i].RecordID = id
		}
		if rows[i].Producer == "" {
			rows[i].Producer = identity.ProducerDesktop
			if rows[i].URL != nil && *rows[i].URL != "" {
				rows[i].Producer = identity.ProducerFirefox
			}
		}
	}
	return rows
}

// newActivityDevice creates a user and one device for it, with names unique per
// call so parallel packages do not collide. The device carries UserID, so the
// user row itself is never what a caller wants back.
func newActivityDevice(t *testing.T, pool *pgxpool.Pool, q *Queries) DeviceRow {
	t.Helper()
	ctx := context.Background()

	stamp := time.Now().UnixNano()
	user, err := q.CreateUser(ctx, fmt.Sprintf("testuser_%d", stamp), "$2a$10$fakehash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupUser(t, pool, user.ID) })

	device, err := q.CreateDevice(ctx, user.ID, "test-laptop", "desktop", fmt.Sprintf("testkey_%d", stamp))
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	return *device
}
