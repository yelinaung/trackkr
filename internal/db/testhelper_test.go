package db

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

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
