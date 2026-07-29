package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yelinaung/trackkr/internal/icon"
)

const (
	// AppIconUserLimit bounds application icon storage for one user.
	AppIconUserLimit = 512
	// SiteIconUserLimit bounds annual favicon cache rows for one user.
	SiteIconUserLimit = 2048
)

type Queries struct {
	pool *pgxpool.Pool
}

func NewQueries(pool *pgxpool.Pool) *Queries {
	return &Queries{pool: pool}
}

func (q *Queries) InsertActivityRecords(ctx context.Context, records []ActivityRecordRow) (int, error) {
	batch := &pgx.Batch{}
	for i := range records {
		r := &records[i]
		batch.Queue(
			`INSERT INTO activity_records (device_id, app_name, title, url, started_at, ended_at, duration_s)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 ON CONFLICT (device_id, started_at) DO NOTHING`,
			r.DeviceID, r.AppName, r.Title, r.URL, r.StartedAt, r.EndedAt, r.DurationS,
		)
	}

	br := q.pool.SendBatch(ctx, batch)
	defer func() { _ = br.Close() }()

	accepted := 0
	for range records {
		ct, err := br.Exec()
		if err != nil {
			return accepted, err
		}
		if ct.RowsAffected() > 0 {
			accepted++
		}
	}
	return accepted, nil
}

func (q *Queries) GetDeviceByAPIKey(ctx context.Context, apiKey string) (*DeviceRow, error) {
	row := q.pool.QueryRow(ctx,
		`SELECT id, user_id, name, device_type, api_key, created_at
		 FROM devices WHERE api_key = $1`, apiKey)

	var d DeviceRow
	err := row.Scan(&d.ID, &d.UserID, &d.Name, &d.DeviceType, &d.APIKey, &d.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (q *Queries) ListDevicesByUser(ctx context.Context, userID int64) ([]DeviceRow, error) {
	rows, err := q.pool.Query(ctx,
		`SELECT id, user_id, name, device_type, api_key, created_at
		 FROM devices WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []DeviceRow
	for rows.Next() {
		var d DeviceRow
		if err := rows.Scan(&d.ID, &d.UserID, &d.Name, &d.DeviceType, &d.APIKey, &d.CreatedAt); err != nil {
			return nil, err
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

const (
	// ActivityRecordLimit caps how many effective records one timeline renders.
	ActivityRecordLimit = 5000
	// ActivitySourceLimit bounds raw records before in-memory deduplication. It
	// is intentionally higher than the display cap so overlap removal does not
	// normally hide later effective records.
	ActivitySourceLimit = 25000
)

// GetActivitySummary reads a bounded source window once, then derives both the
// effective timeline and app totals from the same deduplicated records.
func (q *Queries) GetActivitySummary(
	ctx context.Context,
	userID int64,
	start, end time.Time,
	deviceID *int64,
) (*ActivitySummary, error) {
	records, sourceTruncated, err := q.queryActivityRecords(ctx, userID, start, end, deviceID)
	if err != nil {
		return nil, err
	}

	deduplicator := newActivityDeduplicator(records)
	effective, effectiveTruncated := deduplicator.timeline(
		ActivityRecordLimit,
		activityDedupWorkLimit,
	)
	summary := &ActivitySummary{
		Records:           effective,
		Totals:            deduplicator.totals(start, end),
		TimelineTruncated: sourceTruncated || effectiveTruncated,
		SourceTruncated:   sourceTruncated,
	}
	return summary, nil
}

// queryActivityRecords fetches one probe row past the source limit so callers
// can distinguish an exactly full window from a truncated one.
func (q *Queries) queryActivityRecords(
	ctx context.Context,
	userID int64,
	start, end time.Time,
	deviceID *int64,
) ([]ActivityRecordRow, bool, error) {
	var rows pgx.Rows
	var err error

	if deviceID != nil {
		rows, err = q.pool.Query(ctx,
			`SELECT ar.id, ar.device_id, ar.app_name, ar.title, ar.url, ar.started_at, ar.ended_at, ar.duration_s, ar.created_at
			 FROM activity_records ar
			 JOIN devices d ON d.id = ar.device_id
			 WHERE d.user_id = $1 AND ar.started_at < $3 AND ar.ended_at > $2 AND ar.device_id = $4
			 ORDER BY ar.started_at, ar.device_id, ar.id
			 LIMIT $5`,
			userID, start, end, *deviceID, ActivitySourceLimit+1)
	} else {
		rows, err = q.pool.Query(ctx,
			`SELECT ar.id, ar.device_id, ar.app_name, ar.title, ar.url, ar.started_at, ar.ended_at, ar.duration_s, ar.created_at
			 FROM activity_records ar
			 JOIN devices d ON d.id = ar.device_id
			 WHERE d.user_id = $1 AND ar.started_at < $3 AND ar.ended_at > $2
			 ORDER BY ar.started_at, ar.device_id, ar.id
			 LIMIT $4`,
			userID, start, end, ActivitySourceLimit+1)
	}
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var records []ActivityRecordRow
	for rows.Next() {
		var r ActivityRecordRow
		if err := rows.Scan(&r.ID, &r.DeviceID, &r.AppName, &r.Title, &r.URL,
			&r.StartedAt, &r.EndedAt, &r.DurationS, &r.CreatedAt); err != nil {
			return nil, false, err
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	sourceTruncated := len(records) > ActivitySourceLimit
	if sourceTruncated {
		records = records[:ActivitySourceLimit]
	}
	return records, sourceTruncated, nil
}

// SiteTotalLimit caps how many sites the summary lists.
const SiteTotalLimit = 25

// siteExpr derives a display host from a URL.
//
// It extracts the hostname specifically, not the URL authority. The
// authority also carries userinfo and a port, so grouping on it puts
// credentials on the dashboard -- https://user:password@example.com
// renders the password verbatim -- and splits one site across rows
// for example.com, example.com:443, and EXAMPLE.com.
//
// Innermost first: take the authority, drop any userinfo before "@",
// drop a trailing port while keeping a bracketed IPv6 literal intact,
// lowercase, remove a trailing DNS root dot, then strip a leading "www.".
const siteExpr = `regexp_replace(
	regexp_replace(
		lower(regexp_replace(
			regexp_replace(substring(ar.url from '^[a-z][a-z0-9+.-]*://([^/?#]*)'), '^[^@]*@', ''),
			'^(\[[^\]]+\]|[^:]+)(:[0-9]+)?$', '\1')),
		'\.$', ''),
	'^www\.', '')`

// GetSiteTotals sums browsing time per site within [start, end).
//
// Records from the desktop tracker have no URL and are excluded: the
// question this answers is which pages the time went to, and an app
// with no URL cannot contribute to it.
func (q *Queries) GetSiteTotals(ctx context.Context, userID int64, start, end time.Time, deviceID *int64) ([]SiteTotalRow, error) {
	var rows pgx.Rows
	var err error

	if deviceID != nil {
		rows, err = q.pool.Query(ctx,
			`SELECT `+siteExpr+` AS site,
			        SUM(EXTRACT(EPOCH FROM (LEAST(ar.ended_at, $3) - GREATEST(ar.started_at, $2))))::bigint
			 FROM activity_records ar
			 JOIN devices d ON d.id = ar.device_id
			 WHERE d.user_id = $1 AND ar.started_at < $3 AND ar.ended_at > $2
			   AND ar.ended_at > ar.started_at
			   AND ar.device_id = $4 AND ar.url IS NOT NULL AND ar.url <> ''
			 GROUP BY 1
			 HAVING `+siteExpr+` IS NOT NULL
			 ORDER BY 2 DESC, 1
			 LIMIT $5`,
			userID, start, end, *deviceID, SiteTotalLimit)
	} else {
		rows, err = q.pool.Query(ctx,
			`SELECT `+siteExpr+` AS site,
			        SUM(EXTRACT(EPOCH FROM (LEAST(ar.ended_at, $3) - GREATEST(ar.started_at, $2))))::bigint
			 FROM activity_records ar
			 JOIN devices d ON d.id = ar.device_id
			 WHERE d.user_id = $1 AND ar.started_at < $3 AND ar.ended_at > $2
			   AND ar.ended_at > ar.started_at
			   AND ar.url IS NOT NULL AND ar.url <> ''
			 GROUP BY 1
			 HAVING `+siteExpr+` IS NOT NULL
			 ORDER BY 2 DESC, 1
			 LIMIT $4`,
			userID, start, end, SiteTotalLimit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var totals []SiteTotalRow
	for rows.Next() {
		var t SiteTotalRow
		if err := rows.Scan(&t.Site, &t.Seconds); err != nil {
			return nil, err
		}
		totals = append(totals, t)
	}
	return totals, rows.Err()
}

// SiteIcon returns one user-scoped favicon cache entry.
func (q *Queries) SiteIcon(ctx context.Context, userID int64, site string) (*SiteIconRow, error) {
	return scanSiteIcon(q.pool.QueryRow(ctx,
		`SELECT id, user_id, site, png, sha256, width, height, attempted_at,
		        expires_at, claim_until, created_at, updated_at
		 FROM site_icons
		 WHERE user_id = $1 AND site = $2`,
		userID, site))
}

// ClaimSiteIconRefresh atomically reserves an expired cache entry. The lease
// prevents concurrent dashboard requests from fetching the same site.
func (q *Queries) ClaimSiteIconRefresh(
	ctx context.Context,
	userID int64,
	site string,
	now, claimUntil time.Time,
	repair bool,
) (*SiteIconRow, bool, error) {
	tx, err := q.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("beginning site icon claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockSiteIconUser(ctx, tx, userID); err != nil {
		return nil, false, err
	}

	row, err := scanSiteIcon(tx.QueryRow(ctx,
		`INSERT INTO site_icons (user_id, site, expires_at, claim_until, updated_at)
		 VALUES ($1, $2, $3, $4, $3)
		 ON CONFLICT (user_id, site) DO UPDATE SET
		     claim_until = EXCLUDED.claim_until,
		     updated_at = EXCLUDED.updated_at
			 WHERE ($5 OR site_icons.expires_at <= $3)
			   AND (site_icons.claim_until IS NULL OR site_icons.claim_until <= $3)
		 RETURNING id, user_id, site, png, sha256, width, height, attempted_at,
		           expires_at, claim_until, created_at, updated_at`,
		userID, site, now, claimUntil, repair))
	claimed := err == nil
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, false, err
		}
		row, err = scanSiteIcon(tx.QueryRow(ctx,
			`SELECT id, user_id, site, png, sha256, width, height, attempted_at,
			        expires_at, claim_until, created_at, updated_at
			 FROM site_icons
			 WHERE user_id = $1 AND site = $2`,
			userID, site))
		if err != nil {
			return nil, false, err
		}
	}

	if claimed {
		if err := pruneSiteIcons(ctx, tx, userID); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("committing site icon claim: %w", err)
	}
	return row, claimed, nil
}

// CompleteSiteIconRefresh records either a normalized PNG or a failed annual
// attempt. A failed refresh retains an older icon if one exists.
func (q *Queries) CompleteSiteIconRefresh(
	ctx context.Context,
	userID int64,
	site string,
	pngBytes []byte,
	details *icon.Details,
	attemptedAt, expiresAt, claimUntil time.Time,
) (*SiteIconRow, error) {
	var digest []byte
	var width, height *int
	if details != nil {
		validated, err := icon.ValidatePNG(pngBytes)
		if err != nil {
			return nil, fmt.Errorf("validating completed site icon: %w", err)
		}
		digest = validated.Digest[:]
		width = &validated.Width
		height = &validated.Height
	} else {
		pngBytes = nil
	}

	tx, err := q.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("beginning site icon completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockSiteIconUser(ctx, tx, userID); err != nil {
		return nil, err
	}

	row, err := scanSiteIcon(tx.QueryRow(ctx,
		`UPDATE site_icons SET
		     png = CASE WHEN $4::bytea IS NULL THEN png ELSE $4 END,
		     sha256 = CASE WHEN $4::bytea IS NULL THEN sha256 ELSE $5 END,
		     width = CASE WHEN $4::bytea IS NULL THEN width ELSE $6 END,
		     height = CASE WHEN $4::bytea IS NULL THEN height ELSE $7 END,
		     attempted_at = $8,
		     expires_at = $9,
		     claim_until = NULL,
		     updated_at = $8
		 WHERE user_id = $1 AND site = $2 AND claim_until = $3
		 RETURNING id, user_id, site, png, sha256, width, height, attempted_at,
		           expires_at, claim_until, created_at, updated_at`,
		userID, site, claimUntil, pngBytes, digest, width, height, attemptedAt, expiresAt))
	if err != nil {
		return nil, err
	}
	if err := pruneSiteIcons(ctx, tx, userID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing site icon completion: %w", err)
	}
	return row, nil
}

func lockSiteIconUser(ctx context.Context, tx pgx.Tx, userID int64) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(-$1::bigint)`, userID); err != nil {
		return fmt.Errorf("locking site icon user: %w", err)
	}
	return nil
}

func lockAppIconUser(ctx context.Context, tx pgx.Tx, userID int64) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, userID); err != nil {
		return fmt.Errorf("locking app icon user: %w", err)
	}
	return nil
}

func pruneSiteIcons(ctx context.Context, tx pgx.Tx, userID int64) error {
	_, err := tx.Exec(ctx,
		`WITH excess AS (
		     SELECT id
		     FROM site_icons
		     WHERE user_id = $1
		     ORDER BY updated_at DESC, id DESC
		     OFFSET $2
		 )
		 DELETE FROM site_icons
		 WHERE id IN (SELECT id FROM excess)`,
		userID, SiteIconUserLimit)
	if err != nil {
		return fmt.Errorf("pruning site icons: %w", err)
	}
	return nil
}

type siteIconScanner interface {
	Scan(dest ...any) error
}

func scanSiteIcon(row siteIconScanner) (*SiteIconRow, error) {
	var result SiteIconRow
	if err := row.Scan(
		&result.ID,
		&result.UserID,
		&result.Site,
		&result.PNG,
		&result.SHA256,
		&result.Width,
		&result.Height,
		&result.AttemptedAt,
		&result.ExpiresAt,
		&result.ClaimUntil,
		&result.CreatedAt,
		&result.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &result, nil
}

// UpsertAppIcons stores application icons and prunes the oldest rows above
// AppIconUserLimit. A user-scoped advisory lock serializes retention across
// devices without blocking site favicon retention for the same user.
func (q *Queries) UpsertAppIcons(ctx context.Context, userID int64, apps []icon.App) (int, error) {
	type validatedApp struct {
		app     icon.App
		details icon.Details
	}

	validated := make([]validatedApp, len(apps))
	for i, app := range apps {
		details, err := icon.Validate(app)
		if err != nil {
			return 0, fmt.Errorf("validating icon %d: %w", i, err)
		}
		validated[i] = validatedApp{app: app, details: details}
	}

	tx, err := q.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("beginning app icon transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockAppIconUser(ctx, tx, userID); err != nil {
		return 0, err
	}
	var acceptedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&acceptedAt); err != nil {
		return 0, fmt.Errorf("reading app icon acceptance time: %w", err)
	}

	for _, item := range validated {
		_, err := tx.Exec(
			ctx,
			`INSERT INTO app_icons (
			     user_id, app_key, png, sha256, width, height,
			     created_at, updated_at, last_seen_at
			 )
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $7, $7)
			 ON CONFLICT (user_id, app_key) DO UPDATE SET
			     png = CASE
			         WHEN app_icons.sha256 = EXCLUDED.sha256 THEN app_icons.png
			         ELSE EXCLUDED.png
			     END,
			     sha256 = CASE
			         WHEN app_icons.sha256 = EXCLUDED.sha256 THEN app_icons.sha256
			         ELSE EXCLUDED.sha256
			     END,
			     width = CASE
			         WHEN app_icons.sha256 = EXCLUDED.sha256 THEN app_icons.width
			         ELSE EXCLUDED.width
			     END,
			     height = CASE
			         WHEN app_icons.sha256 = EXCLUDED.sha256 THEN app_icons.height
			         ELSE EXCLUDED.height
			     END,
			     updated_at = CASE
			         WHEN app_icons.sha256 = EXCLUDED.sha256 THEN app_icons.updated_at
			         ELSE $7
			     END,
			     last_seen_at = $7`,
			userID,
			item.app.Key,
			item.app.PNG,
			item.details.Digest[:],
			item.details.Width,
			item.details.Height,
			acceptedAt,
		)
		if err != nil {
			return 0, fmt.Errorf("upserting app icon %q: %w", item.app.Key, err)
		}
	}

	result, err := tx.Exec(ctx,
		`WITH excess AS (
		     SELECT id
		     FROM app_icons
		     WHERE user_id = $1
		     ORDER BY last_seen_at DESC, id DESC
		     OFFSET $2
		 )
		 DELETE FROM app_icons
		 WHERE id IN (SELECT id FROM excess)`,
		userID, AppIconUserLimit)
	if err != nil {
		return 0, fmt.Errorf("pruning app icons: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing app icons: %w", err)
	}
	return int(result.RowsAffected()), nil
}

// AppIconMetadata returns icon metadata without loading PNG bytes.
func (q *Queries) AppIconMetadata(ctx context.Context, userID int64, keys []string) ([]AppIconRow, error) {
	if len(keys) == 0 {
		return nil, nil
	}

	rows, err := q.pool.Query(ctx,
		`SELECT id, user_id, app_key, sha256, width, height,
		        created_at, updated_at, last_seen_at
		 FROM app_icons
		 WHERE user_id = $1 AND app_key = ANY($2)
		 ORDER BY app_key`,
		userID, keys)
	if err != nil {
		return nil, fmt.Errorf("querying app icon metadata: %w", err)
	}
	defer rows.Close()

	var result []AppIconRow
	for rows.Next() {
		var row AppIconRow
		if err := rows.Scan(
			&row.ID,
			&row.UserID,
			&row.AppKey,
			&row.SHA256,
			&row.Width,
			&row.Height,
			&row.CreatedAt,
			&row.UpdatedAt,
			&row.LastSeenAt,
		); err != nil {
			return nil, fmt.Errorf("scanning app icon metadata: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading app icon metadata: %w", err)
	}
	return result, nil
}

// AppIcon returns one user-owned icon including its PNG bytes.
func (q *Queries) AppIcon(ctx context.Context, userID, id int64) (*AppIconRow, error) {
	var row AppIconRow
	err := q.pool.QueryRow(
		ctx,
		`SELECT id, user_id, app_key, png, sha256, width, height,
		        created_at, updated_at, last_seen_at
		 FROM app_icons
		 WHERE user_id = $1 AND id = $2`,
		userID, id,
	).Scan(
		&row.ID,
		&row.UserID,
		&row.AppKey,
		&row.PNG,
		&row.SHA256,
		&row.Width,
		&row.Height,
		&row.CreatedAt,
		&row.UpdatedAt,
		&row.LastSeenAt,
	)
	if err != nil {
		return nil, fmt.Errorf("querying app icon: %w", err)
	}
	return &row, nil
}

func (q *Queries) GetUserByID(ctx context.Context, id int64) (*UserRow, error) {
	row := q.pool.QueryRow(ctx,
		`SELECT id, username, password, created_at FROM users WHERE id = $1`, id)

	var u UserRow
	if err := row.Scan(&u.ID, &u.Username, &u.Password, &u.CreatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}

func (q *Queries) GetUserByUsername(ctx context.Context, username string) (*UserRow, error) {
	row := q.pool.QueryRow(ctx,
		`SELECT id, username, password, created_at FROM users WHERE username = $1`, username)

	var u UserRow
	if err := row.Scan(&u.ID, &u.Username, &u.Password, &u.CreatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}

func (q *Queries) CreateDevice(ctx context.Context, userID int64, name, deviceType, apiKey string) (*DeviceRow, error) {
	row := q.pool.QueryRow(ctx,
		`INSERT INTO devices (user_id, name, device_type, api_key)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, user_id, name, device_type, api_key, created_at`,
		userID, name, deviceType, apiKey)

	var d DeviceRow
	if err := row.Scan(&d.ID, &d.UserID, &d.Name, &d.DeviceType, &d.APIKey, &d.CreatedAt); err != nil {
		return nil, err
	}
	return &d, nil
}

func (q *Queries) DeleteDevice(ctx context.Context, id int64, userID int64) error {
	_, err := q.pool.Exec(ctx,
		`DELETE FROM devices WHERE id = $1 AND user_id = $2`, id, userID)
	return err
}

func (q *Queries) CreateUser(ctx context.Context, username, passwordHash string) (*UserRow, error) {
	row := q.pool.QueryRow(ctx,
		`INSERT INTO users (username, password)
		 VALUES ($1, $2)
		 RETURNING id, username, password, created_at`,
		username, passwordHash)

	var u UserRow
	if err := row.Scan(&u.ID, &u.Username, &u.Password, &u.CreatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}
