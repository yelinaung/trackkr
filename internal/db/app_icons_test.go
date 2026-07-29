package db

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yelinaung/trackkr/internal/icon"
)

const testFinderIconKey = "finder"

func TestUpsertAppIcons(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := t.Context()
	user := createIconTestUser(t, q, pool)

	first := icon.App{Key: testFinderIconKey, PNG: dbTestPNG(t, color.NRGBA{R: 0x11, A: 0xff})}
	if evicted, err := q.UpsertAppIcons(ctx, user.ID, []icon.App{first}); err != nil {
		t.Fatalf("UpsertAppIcons: %v", err)
	} else if evicted != 0 {
		t.Errorf("evicted = %d, want 0", evicted)
	}

	stored, err := q.AppIconMetadata(ctx, user.ID, []string{testFinderIconKey})
	if err != nil {
		t.Fatalf("AppIconMetadata: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("metadata rows = %d, want 1", len(stored))
	}
	if stored[0].PNG != nil {
		t.Errorf("metadata PNG = %d bytes, want nil", len(stored[0].PNG))
	}

	oldTime := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(
		ctx,
		`UPDATE app_icons SET updated_at = $1, last_seen_at = $1 WHERE id = $2`,
		oldTime, stored[0].ID,
	); err != nil {
		t.Fatalf("backdating icon: %v", err)
	}

	if _, err := q.UpsertAppIcons(ctx, user.ID, []icon.App{first}); err != nil {
		t.Fatalf("replaying identical icon: %v", err)
	}
	identical, err := q.AppIcon(ctx, user.ID, stored[0].ID)
	if err != nil {
		t.Fatalf("AppIcon after replay: %v", err)
	}
	if !identical.UpdatedAt.Equal(oldTime) {
		t.Errorf("identical updated_at = %v, want %v", identical.UpdatedAt, oldTime)
	}
	if !identical.LastSeenAt.After(oldTime) {
		t.Errorf("identical last_seen_at = %v, want after %v", identical.LastSeenAt, oldTime)
	}

	changed := icon.App{Key: testFinderIconKey, PNG: dbTestPNG(t, color.NRGBA{G: 0x88, A: 0xff})}
	if _, err := q.UpsertAppIcons(ctx, user.ID, []icon.App{changed}); err != nil {
		t.Fatalf("updating changed icon: %v", err)
	}
	updated, err := q.AppIcon(ctx, user.ID, stored[0].ID)
	if err != nil {
		t.Fatalf("AppIcon after update: %v", err)
	}
	if !updated.UpdatedAt.After(oldTime) {
		t.Errorf("changed updated_at = %v, want after %v", updated.UpdatedAt, oldTime)
	}
	if bytes.Equal(updated.SHA256, identical.SHA256) {
		t.Error("changed icon retained the old digest")
	}
	if !bytes.Equal(updated.PNG, changed.PNG) {
		t.Error("changed icon retained the old PNG")
	}
}

func TestAppIconsAreUserScopedAndCascade(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	ctx := t.Context()
	owner := createIconTestUser(t, q, pool)
	other := createIconTestUser(t, q, pool)

	app := icon.App{Key: "safari", PNG: dbTestPNG(t, color.NRGBA{B: 0xcc, A: 0xff})}
	if _, err := q.UpsertAppIcons(ctx, owner.ID, []icon.App{app}); err != nil {
		t.Fatalf("UpsertAppIcons: %v", err)
	}
	metadata, err := q.AppIconMetadata(ctx, owner.ID, []string{app.Key})
	if err != nil || len(metadata) != 1 {
		t.Fatalf("AppIconMetadata = %#v, %v", metadata, err)
	}

	if _, err := q.AppIcon(ctx, other.ID, metadata[0].ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("cross-user AppIcon error = %v, want pgx.ErrNoRows", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, owner.ID); err != nil {
		t.Fatalf("deleting owner: %v", err)
	}
	var count int
	if err := pool.QueryRow(
		ctx,
		`SELECT COUNT(*) FROM app_icons WHERE user_id = $1`, owner.ID,
	).Scan(&count); err != nil {
		t.Fatalf("counting cascaded icons: %v", err)
	}
	if count != 0 {
		t.Errorf("icons after user deletion = %d, want 0", count)
	}
}

func TestUpsertAppIconsPrunesToUserLimit(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	user := createIconTestUser(t, q, pool)
	pngBytes := dbTestPNG(t, color.NRGBA{R: 0xaa, G: 0x55, A: 0xff})

	apps := make([]icon.App, AppIconUserLimit+3)
	for i := range apps {
		apps[i] = icon.App{Key: fmt.Sprintf("app-%03d", i), PNG: pngBytes}
	}
	evicted, err := q.UpsertAppIcons(t.Context(), user.ID, apps)
	if err != nil {
		t.Fatalf("UpsertAppIcons: %v", err)
	}
	if evicted != 3 {
		t.Errorf("evicted = %d, want 3", evicted)
	}
	if got := countUserIcons(t, pool, user.ID); got != AppIconUserLimit {
		t.Errorf("stored icons = %d, want %d", got, AppIconUserLimit)
	}
}

func TestConcurrentUpsertAppIconsPreservesUserLimit(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	user := createIconTestUser(t, q, pool)
	pngBytes := dbTestPNG(t, color.NRGBA{G: 0xaa, B: 0x55, A: 0xff})

	const perDevice = 300
	ready := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for device := range 2 {
		apps := make([]icon.App, perDevice)
		for i := range apps {
			apps[i] = icon.App{
				Key: fmt.Sprintf("device-%d-app-%03d", device, i),
				PNG: pngBytes,
			}
		}
		wg.Go(func() {
			<-ready
			_, err := q.UpsertAppIcons(t.Context(), user.ID, apps)
			errs <- err
		})
	}
	close(ready)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent UpsertAppIcons: %v", err)
		}
	}

	if got := countUserIcons(t, pool, user.ID); got != AppIconUserLimit {
		t.Errorf("stored icons = %d, want %d", got, AppIconUserLimit)
	}
}

func TestUpsertAppIconsTimestampsAfterUserLock(t *testing.T) {
	pool := testPool(t)
	q := NewQueries(pool)
	user := createIconTestUser(t, q, pool)
	pngBytes := dbTestPNG(t, color.NRGBA{R: 0x77, G: 0x44, A: 0xff})

	apps := make([]icon.App, AppIconUserLimit)
	for i := range apps {
		apps[i] = icon.App{Key: fmt.Sprintf("existing-%03d", i), PNG: pngBytes}
	}
	if _, err := q.UpsertAppIcons(t.Context(), user.ID, apps); err != nil {
		t.Fatalf("filling icon retention limit: %v", err)
	}

	blocker, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("beginning blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if err := lockAppIconUser(t.Context(), blocker, user.ID); err != nil {
		t.Fatalf("locking app icons: %v", err)
	}

	accepted := icon.App{Key: "waited-new", PNG: pngBytes}
	type upsertResult struct {
		evicted int
		err     error
	}
	result := make(chan upsertResult, 1)
	go func() {
		evicted, upsertErr := q.UpsertAppIcons(t.Context(), user.ID, []icon.App{accepted})
		result <- upsertResult{evicted: evicted, err: upsertErr}
	}()
	waitForAppIconUserLock(t, pool)

	var lockHolderTime time.Time
	if err := blocker.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&lockHolderTime); err != nil {
		t.Fatalf("reading lock-holder time: %v", err)
	}
	if _, err := blocker.Exec(
		t.Context(),
		`UPDATE app_icons
		 SET updated_at = $2, last_seen_at = $2
		 WHERE user_id = $1`,
		user.ID, lockHolderTime,
	); err != nil {
		t.Fatalf("advancing existing icon timestamps: %v", err)
	}
	if err := blocker.Commit(t.Context()); err != nil {
		t.Fatalf("committing blocker: %v", err)
	}

	got := <-result
	if got.err != nil {
		t.Fatalf("waiting UpsertAppIcons: %v", got.err)
	}
	if got.evicted != 1 {
		t.Errorf("evicted = %d, want 1", got.evicted)
	}
	rows, err := q.AppIconMetadata(t.Context(), user.ID, []string{accepted.Key})
	if err != nil {
		t.Fatalf("loading accepted icon: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("accepted icon rows = %d, want 1", len(rows))
	}
	if !rows[0].LastSeenAt.After(lockHolderTime) {
		t.Errorf(
			"accepted last_seen_at = %v, want after lock-holder time %v",
			rows[0].LastSeenAt,
			lockHolderTime,
		)
	}
}

func waitForAppIconUserLock(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := pool.QueryRow(
			ctx,
			`SELECT EXISTS (
			     SELECT 1
			     FROM pg_stat_activity
			     WHERE pid <> pg_backend_pid()
			       AND datname = current_database()
			       AND state = 'active'
			       AND wait_event_type = 'Lock'
			       AND query LIKE '%pg_advisory_xact_lock($1::bigint)%'
			 )`,
		).Scan(&waiting)
		if err != nil {
			t.Fatalf("checking app icon lock wait: %v", err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for app icon user lock")
		case <-ticker.C:
		}
	}
}

func createIconTestUser(t *testing.T, q *Queries, pool *pgxpool.Pool) *UserRow {
	t.Helper()

	user, err := q.CreateUser(
		t.Context(),
		fmt.Sprintf("icon_user_%d", time.Now().UnixNano()),
		"$2a$10$fakehash",
	)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, user.ID)
	})
	return user
}

func countUserIcons(t *testing.T, pool *pgxpool.Pool, userID int64) int {
	t.Helper()

	var count int
	if err := pool.QueryRow(
		t.Context(), `SELECT COUNT(*) FROM app_icons WHERE user_id = $1`, userID,
	).Scan(&count); err != nil {
		t.Fatalf("counting app icons: %v", err)
	}
	return count
}

func dbTestPNG(tb testing.TB, fill color.NRGBA) []byte {
	tb.Helper()

	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for y := range 2 {
		for x := range 2 {
			img.SetNRGBA(x, y, fill)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		tb.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}
