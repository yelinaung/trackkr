package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yelinaung/trackkr/internal/icon"
)

// AppIconUserLimit bounds application icon storage for one user.
const AppIconUserLimit = 512

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

// ActivityRecordLimit caps how many records one timeline page renders.
// The cap truncates the end of the day rather than sampling it.
//
// GetActivityRecords fetches one row beyond the limit so the caller can
// tell "exactly full" from "truncated": comparing len(records) against
// the limit alone reports a truncated chart for a day that fit exactly.
const ActivityRecordLimit = 5000

// GetActivityRecords returns records overlapping [start, end). Selecting
// on overlap rather than on started_at is what makes a record spanning
// midnight visible on both days.
func (q *Queries) GetActivityRecords(ctx context.Context, userID int64, start, end time.Time, deviceID *int64) ([]ActivityRecordRow, error) {
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
			userID, start, end, *deviceID, ActivityRecordLimit+1)
	} else {
		rows, err = q.pool.Query(ctx,
			`SELECT ar.id, ar.device_id, ar.app_name, ar.title, ar.url, ar.started_at, ar.ended_at, ar.duration_s, ar.created_at
			 FROM activity_records ar
			 JOIN devices d ON d.id = ar.device_id
			 WHERE d.user_id = $1 AND ar.started_at < $3 AND ar.ended_at > $2
			 ORDER BY ar.started_at, ar.device_id, ar.id
			 LIMIT $4`,
			userID, start, end, ActivityRecordLimit+1)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []ActivityRecordRow
	for rows.Next() {
		var r ActivityRecordRow
		if err := rows.Scan(&r.ID, &r.DeviceID, &r.AppName, &r.Title, &r.URL,
			&r.StartedAt, &r.EndedAt, &r.DurationS, &r.CreatedAt); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// GetAppTotals sums per-app time within [start, end). It sums the part
// of each record that falls inside the window, so a record spanning
// midnight is not counted in full on both days.
func (q *Queries) GetAppTotals(ctx context.Context, userID int64, start, end time.Time, deviceID *int64) ([]AppTotalRow, error) {
	var rows pgx.Rows
	var err error

	if deviceID != nil {
		rows, err = q.pool.Query(ctx,
			`SELECT ar.app_name,
			        SUM(EXTRACT(EPOCH FROM (LEAST(ar.ended_at, $3) - GREATEST(ar.started_at, $2))))::bigint
			 FROM activity_records ar
			 JOIN devices d ON d.id = ar.device_id
			 WHERE d.user_id = $1 AND ar.started_at < $3 AND ar.ended_at > $2 AND ar.device_id = $4
			 GROUP BY ar.app_name
			 ORDER BY 2 DESC, 1`,
			userID, start, end, *deviceID)
	} else {
		rows, err = q.pool.Query(ctx,
			`SELECT ar.app_name,
			        SUM(EXTRACT(EPOCH FROM (LEAST(ar.ended_at, $3) - GREATEST(ar.started_at, $2))))::bigint
			 FROM activity_records ar
			 JOIN devices d ON d.id = ar.device_id
			 WHERE d.user_id = $1 AND ar.started_at < $3 AND ar.ended_at > $2
			 GROUP BY ar.app_name
			 ORDER BY 2 DESC, 1`,
			userID, start, end)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var totals []AppTotalRow
	for rows.Next() {
		var t AppTotalRow
		if err := rows.Scan(&t.AppName, &t.Seconds); err != nil {
			return nil, err
		}
		totals = append(totals, t)
	}
	return totals, rows.Err()
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
// lowercase, then strip a leading "www.".
const siteExpr = `regexp_replace(
	lower(regexp_replace(
		regexp_replace(substring(ar.url from '^[a-z][a-z0-9+.-]*://([^/?#]*)'), '^[^@]*@', ''),
		'^(\[[^\]]+\]|[^:]+)(:[0-9]+)?$', '\1')),
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

// UpsertAppIcons stores application icons and prunes the oldest rows above
// AppIconUserLimit. Locking the user row serializes retention across devices.
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

	var lockedUserID int64
	if err := tx.QueryRow(
		ctx,
		`SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID,
	).Scan(&lockedUserID); err != nil {
		return 0, fmt.Errorf("locking app icon user: %w", err)
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
