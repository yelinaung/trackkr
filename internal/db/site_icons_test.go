package db

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"sync"
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/icon"
)

func TestSiteIconAnnualRefreshLifecycle(t *testing.T) {
	pool := testPool(t)
	queries := NewQueries(pool)
	user, _ := seedUserAndDevice(t, pool, queries)

	now := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	claimUntil := now.Add(time.Minute)
	row, claimed, err := queries.ClaimSiteIconRefresh(
		context.Background(), user.ID, "example.com", now, claimUntil, false,
	)
	if err != nil {
		t.Fatalf("ClaimSiteIconRefresh: %v", err)
	}
	if !claimed || row.ClaimUntil == nil || !row.ClaimUntil.Equal(claimUntil) {
		t.Fatalf("claim = (%v, %+v), want claimed lease", claimed, row)
	}

	if _, secondClaim, err := queries.ClaimSiteIconRefresh(
		context.Background(), user.ID, "example.com", now.Add(time.Second), now.Add(2*time.Minute), false,
	); err != nil {
		t.Fatalf("second ClaimSiteIconRefresh: %v", err)
	} else if secondClaim {
		t.Fatal("second request claimed an in-flight site")
	}

	pngBytes := databaseSiteIconPNG(t)
	details, err := icon.ValidatePNG(pngBytes)
	if err != nil {
		t.Fatalf("ValidatePNG: %v", err)
	}
	expiresAt := now.AddDate(1, 0, 0)
	row, err = queries.CompleteSiteIconRefresh(
		context.Background(), user.ID, "example.com", pngBytes, &details, now, expiresAt, claimUntil,
	)
	if err != nil {
		t.Fatalf("CompleteSiteIconRefresh: %v", err)
	}
	if !bytes.Equal(row.PNG, pngBytes) || !row.ExpiresAt.Equal(expiresAt) || row.ClaimUntil != nil {
		t.Errorf("completed row = %+v", row)
	}

	if _, claimed, err := queries.ClaimSiteIconRefresh(
		context.Background(), user.ID, "example.com", now.Add(364*24*time.Hour), now.AddDate(1, 0, 0), false,
	); err != nil {
		t.Fatalf("fresh ClaimSiteIconRefresh: %v", err)
	} else if claimed {
		t.Fatal("fresh annual entry was claimed")
	}
}

func TestSiteIconRepairClaimBypassesFreshExpiry(t *testing.T) {
	pool := testPool(t)
	queries := NewQueries(pool)
	user, _ := seedUserAndDevice(t, pool, queries)
	now := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)

	if _, err := pool.Exec(
		t.Context(),
		`INSERT INTO site_icons (user_id, site, expires_at, updated_at)
		 VALUES ($1, $2, $3, $4)`,
		user.ID, "repair.example", now.AddDate(1, 0, 0), now,
	); err != nil {
		t.Fatalf("seeding fresh site icon: %v", err)
	}

	claimUntil := now.Add(time.Minute)
	if _, claimed, err := queries.ClaimSiteIconRefresh(
		t.Context(), user.ID, "repair.example", now, claimUntil, true,
	); err != nil {
		t.Fatalf("repair claim: %v", err)
	} else if !claimed {
		t.Fatal("repair did not claim a fresh row")
	}
}

func TestSiteIconFailedRefreshRetainsStalePNG(t *testing.T) {
	pool := testPool(t)
	queries := NewQueries(pool)
	user, _ := seedUserAndDevice(t, pool, queries)
	ctx := context.Background()

	now := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	firstClaim := now.Add(time.Minute)
	if _, claimed, err := queries.ClaimSiteIconRefresh(ctx, user.ID, "example.com", now, firstClaim, false); err != nil || !claimed {
		t.Fatalf("initial claim = %v, %v", claimed, err)
	}
	pngBytes := databaseSiteIconPNG(t)
	details, err := icon.ValidatePNG(pngBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CompleteSiteIconRefresh(
		ctx, user.ID, "example.com", pngBytes, &details, now, now.AddDate(1, 0, 0), firstClaim,
	); err != nil {
		t.Fatal(err)
	}

	refreshAt := now.AddDate(1, 0, 1)
	if _, err := pool.Exec(ctx,
		`UPDATE site_icons SET expires_at = $1 WHERE user_id = $2 AND site = $3`,
		refreshAt.Add(-time.Minute), user.ID, "example.com"); err != nil {
		t.Fatalf("expiring site icon: %v", err)
	}
	secondClaim := refreshAt.Add(time.Minute)
	if _, claimed, err := queries.ClaimSiteIconRefresh(ctx, user.ID, "example.com", refreshAt, secondClaim, false); err != nil || !claimed {
		t.Fatalf("refresh claim = %v, %v", claimed, err)
	}

	row, err := queries.CompleteSiteIconRefresh(
		ctx, user.ID, "example.com", nil, nil, refreshAt, refreshAt.AddDate(1, 0, 0), secondClaim,
	)
	if err != nil {
		t.Fatalf("failed CompleteSiteIconRefresh: %v", err)
	}
	if !bytes.Equal(row.PNG, pngBytes) {
		t.Error("failed refresh discarded the stale favicon")
	}
}

func TestSiteIconCompletionIgnoresBytesWithoutDetails(t *testing.T) {
	pool := testPool(t)
	queries := NewQueries(pool)
	user, _ := seedUserAndDevice(t, pool, queries)
	ctx := t.Context()

	now := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	claimUntil := now.Add(time.Minute)
	if _, claimed, err := queries.ClaimSiteIconRefresh(
		ctx, user.ID, "partial.example", now, claimUntil, false,
	); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}

	row, err := queries.CompleteSiteIconRefresh(
		ctx,
		user.ID,
		"partial.example",
		databaseSiteIconPNG(t),
		nil,
		now,
		now.AddDate(1, 0, 0),
		claimUntil,
	)
	if err != nil {
		t.Fatalf("CompleteSiteIconRefresh: %v", err)
	}
	if len(row.PNG) != 0 || len(row.SHA256) != 0 {
		t.Error("unvalidated bytes were written without digest metadata")
	}
}

func TestConcurrentSiteIconClaimsPreserveUserLimit(t *testing.T) {
	pool := testPool(t)
	queries := NewQueries(pool)
	user, _ := seedUserAndDevice(t, pool, queries)
	ctx := t.Context()
	now := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)

	_, err := pool.Exec(ctx,
		`INSERT INTO site_icons (user_id, site, expires_at, updated_at)
		 SELECT $1, 'old-' || n || '.example', $2::timestamptz,
		        $3::timestamptz - n * interval '1 second'
		 FROM generate_series(1, $4) AS n`,
		user.ID, now.AddDate(1, 0, 0), now, SiteIconUserLimit-1)
	if err != nil {
		t.Fatalf("seeding site icons: %v", err)
	}

	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	var wait sync.WaitGroup
	for _, site := range []string{"new-one.example", "new-two.example"} {
		wait.Go(func() {
			<-start
			_, claimed, claimErr := queries.ClaimSiteIconRefresh(
				ctx, user.ID, site, now, now.Add(time.Minute), false,
			)
			if claimErr != nil {
				errorsCh <- claimErr
				return
			}
			if !claimed {
				errorsCh <- errors.New("new site was not claimed")
			}
		})
	}
	close(start)
	wait.Wait()
	close(errorsCh)
	for claimErr := range errorsCh {
		t.Fatal(claimErr)
	}

	var count int
	if err := pool.QueryRow(
		ctx,
		`SELECT count(*) FROM site_icons WHERE user_id = $1`, user.ID,
	).Scan(&count); err != nil {
		t.Fatalf("counting site icons: %v", err)
	}
	if count != SiteIconUserLimit {
		t.Errorf("rows = %d, want cap %d", count, SiteIconUserLimit)
	}
	for _, site := range []string{"new-one.example", "new-two.example"} {
		if _, err := queries.SiteIcon(ctx, user.ID, site); err != nil {
			t.Errorf("new row %q was pruned: %v", site, err)
		}
	}
}

func TestConcurrentSiteIconClaimsHaveOneWinner(t *testing.T) {
	pool := testPool(t)
	queries := NewQueries(pool)
	user, _ := seedUserAndDevice(t, pool, queries)

	now := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	start := make(chan struct{})
	results := make(chan bool, 2)
	errorsCh := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Go(func() {
			<-start
			_, claimed, err := queries.ClaimSiteIconRefresh(
				context.Background(), user.ID, "concurrent.example", now, now.Add(time.Minute), false,
			)
			if err != nil {
				errorsCh <- err
				return
			}
			results <- claimed
		})
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("ClaimSiteIconRefresh: %v", err)
	}
	winners := 0
	for claimed := range results {
		if claimed {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("claim winners = %d, want 1", winners)
	}
}

func TestAppAndSiteIconLocksAreIndependent(t *testing.T) {
	pool := testPool(t)
	queries := NewQueries(pool)
	user, _ := seedUserAndDevice(t, pool, queries)

	appTx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("beginning app icon transaction: %v", err)
	}
	defer func() { _ = appTx.Rollback(t.Context()) }()
	if err := lockAppIconUser(t.Context(), appTx, user.ID); err != nil {
		t.Fatal(err)
	}

	siteTx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("beginning site icon transaction: %v", err)
	}
	defer func() { _ = siteTx.Rollback(t.Context()) }()
	lockCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := lockSiteIconUser(lockCtx, siteTx, user.ID); err != nil {
		t.Fatalf("site icon lock blocked behind app icon lock: %v", err)
	}
}

func databaseSiteIconPNG(t *testing.T) []byte {
	t.Helper()
	canvas := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := range 32 {
		for x := range 32 {
			canvas.SetNRGBA(x, y, color.NRGBA{R: 0x1f, G: 0x6f, B: 0x5f, A: 0xff})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, canvas); err != nil {
		t.Fatalf("encoding PNG: %v", err)
	}
	return output.Bytes()
}
