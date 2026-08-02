package db

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yelinaung/trackkr/internal/icon"
	"github.com/yelinaung/trackkr/internal/identity"
)

const (
	// AppIconUserLimit bounds application icon storage for one user.
	AppIconUserLimit = 512
	// SiteIconUserLimit bounds annual favicon cache rows for one user.
	SiteIconUserLimit = 2048
	// CategoryUserLimit bounds the category controls and dashboard summary.
	CategoryUserLimit = 50
	// KnownApplicationLimit bounds one category management page.
	KnownApplicationLimit = 500
	// EditableActivityRecordLimit bounds one record-editor page.
	EditableActivityRecordLimit = 100
)

var ErrCategoryLimit = errors.New("category limit reached")

type Queries struct {
	pool *pgxpool.Pool
}

func NewQueries(pool *pgxpool.Pool) *Queries {
	return &Queries{pool: pool}
}

func (q *Queries) ListCategories(ctx context.Context, userID int64) ([]CategorySummaryRow, error) {
	rows, err := q.pool.Query(ctx,
		`SELECT c.id, c.user_id, c.name, c.color_key, c.created_at, c.updated_at,
		        COUNT(a.category_id)
		 FROM categories c
		 LEFT JOIN application_category_assignments a ON a.category_id = c.id
		 WHERE c.user_id = $1
		 GROUP BY c.id
		 ORDER BY c.name, c.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var categories []CategorySummaryRow
	for rows.Next() {
		var category CategorySummaryRow
		if err := rows.Scan(
			&category.ID,
			&category.UserID,
			&category.Name,
			&category.ColorKey,
			&category.CreatedAt,
			&category.UpdatedAt,
			&category.AssignedAppCount,
		); err != nil {
			return nil, err
		}
		categories = append(categories, category)
	}
	return categories, rows.Err()
}

// listCategoryRows loads category metadata for aggregation. Unlike the
// management-page query it deliberately avoids assignment counts.
func (q *Queries) listCategoryRows(ctx context.Context, userID int64) ([]CategoryRow, error) {
	rows, err := q.pool.Query(ctx,
		`SELECT id, user_id, name, color_key, created_at, updated_at
		 FROM categories
		 WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var categories []CategoryRow
	for rows.Next() {
		var category CategoryRow
		if err := rows.Scan(
			&category.ID,
			&category.UserID,
			&category.Name,
			&category.ColorKey,
			&category.CreatedAt,
			&category.UpdatedAt,
		); err != nil {
			return nil, err
		}
		categories = append(categories, category)
	}
	return categories, rows.Err()
}

func (q *Queries) CreateCategory(
	ctx context.Context,
	userID int64,
	name, colorKey string,
) (*CategoryRow, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning category transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var lockedUserID int64
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&lockedUserID); err != nil {
		return nil, err
	}

	var count int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM categories WHERE user_id = $1`, userID).Scan(&count); err != nil {
		return nil, err
	}
	if count >= CategoryUserLimit {
		return nil, ErrCategoryLimit
	}

	var category CategoryRow
	err = tx.QueryRow(
		ctx,
		`INSERT INTO categories (user_id, name, color_key)
		 VALUES ($1, $2, $3)
		 RETURNING id, user_id, name, color_key, created_at, updated_at`,
		userID, name, colorKey,
	).Scan(
		&category.ID,
		&category.UserID,
		&category.Name,
		&category.ColorKey,
		&category.CreatedAt,
		&category.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing category transaction: %w", err)
	}
	return &category, nil
}

func (q *Queries) UpdateCategory(
	ctx context.Context,
	userID, categoryID int64,
	name, colorKey string,
) (*CategoryRow, error) {
	var category CategoryRow
	err := q.pool.QueryRow(
		ctx,
		`UPDATE categories
		 SET name = $3, color_key = $4, updated_at = NOW()
		 WHERE id = $2 AND user_id = $1
		 RETURNING id, user_id, name, color_key, created_at, updated_at`,
		userID, categoryID, name, colorKey,
	).Scan(
		&category.ID,
		&category.UserID,
		&category.Name,
		&category.ColorKey,
		&category.CreatedAt,
		&category.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &category, nil
}

func (q *Queries) DeleteCategory(ctx context.Context, userID, categoryID int64) error {
	ct, err := q.pool.Exec(ctx,
		`DELETE FROM categories WHERE id = $2 AND user_id = $1`, userID, categoryID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (q *Queries) ListAppCategoryAssignments(
	ctx context.Context,
	userID int64,
	appKeys []string,
) (map[string]CategoryRow, error) {
	if len(appKeys) == 0 {
		return map[string]CategoryRow{}, nil
	}

	rows, err := q.pool.Query(ctx,
		`SELECT a.app_key, c.id, c.user_id, c.name, c.color_key, c.created_at, c.updated_at
		 FROM application_category_assignments a
		 JOIN categories c ON c.id = a.category_id AND c.user_id = a.user_id
		 WHERE a.user_id = $1 AND a.app_key = ANY($2)`, userID, appKeys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	assignments := make(map[string]CategoryRow, len(appKeys))
	for rows.Next() {
		var appKey string
		var category CategoryRow
		if err := rows.Scan(
			&appKey,
			&category.ID,
			&category.UserID,
			&category.Name,
			&category.ColorKey,
			&category.CreatedAt,
			&category.UpdatedAt,
		); err != nil {
			return nil, err
		}
		assignments[appKey] = category
	}
	return assignments, rows.Err()
}

func (q *Queries) SetAppCategory(
	ctx context.Context,
	userID int64,
	appKey string,
	categoryID *int64,
) error {
	return setAppCategory(ctx, q.pool, userID, appKey, categoryID)
}

type categoryAssignmentExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func setAppCategory(
	ctx context.Context,
	executor categoryAssignmentExecutor,
	userID int64,
	appKey string,
	categoryID *int64,
) error {
	if categoryID == nil {
		_, err := executor.Exec(ctx,
			`DELETE FROM application_category_assignments
			 WHERE user_id = $1 AND app_key = $2`, userID, appKey)
		return err
	}

	var storedKey string
	err := executor.QueryRow(
		ctx,
		`INSERT INTO application_category_assignments (
		     user_id, app_key, category_id, created_at, updated_at
		 )
		 SELECT $1, $2, c.id, NOW(), NOW()
		 FROM categories c
		 WHERE c.id = $3 AND c.user_id = $1
		 ON CONFLICT (user_id, app_key) DO UPDATE SET
		     category_id = EXCLUDED.category_id,
		     updated_at = EXCLUDED.updated_at
		 RETURNING app_key`,
		userID, appKey, *categoryID,
	).Scan(&storedKey)
	if err != nil {
		return err
	}
	return nil
}

// ListKnownApplications returns at most limit recently seen raw application
// pairs folded to their canonical application identity. The time predicate
// bounds the history scanned; the result limit bounds the grouped output.
func (q *Queries) ListKnownApplications(
	ctx context.Context,
	userID int64,
	since time.Time,
	limit int,
) ([]KnownApplicationRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	if limit > KnownApplicationLimit {
		limit = KnownApplicationLimit
	}

	rows, err := q.pool.Query(ctx,
		`SELECT ar.producer, ar.app_name, MAX(ar.ended_at)
		 FROM activity_records ar
		 JOIN devices d ON d.id = ar.device_id
		 WHERE d.user_id = $1 AND ar.ended_at >= $2
		 GROUP BY ar.producer, ar.app_name
		 ORDER BY MAX(ar.ended_at) DESC, ar.producer, ar.app_name
		 LIMIT $3`,
		userID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byKey := make(map[string]KnownApplicationRow)
	for rows.Next() {
		var producer identity.Producer
		var appName string
		var lastSeen time.Time
		if err := rows.Scan(&producer, &appName, &lastSeen); err != nil {
			return nil, err
		}
		canonical := canonicalAppName(producer, appName)
		appKey := CategoryAppKey(canonical)
		if appKey == "" {
			continue
		}
		row, exists := byKey[appKey]
		if !exists || lastSeen.After(row.LastSeen) {
			byKey[appKey] = KnownApplicationRow{
				AppKey:   appKey,
				AppName:  canonical,
				LastSeen: lastSeen,
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := make([]KnownApplicationRow, 0, len(byKey))
	for _, row := range byKey {
		result = append(result, row)
	}
	slices.SortFunc(result, func(a, b KnownApplicationRow) int {
		if order := b.LastSeen.Compare(a.LastSeen); order != 0 {
			return order
		}
		return cmp.Compare(a.AppName, b.AppName)
	})
	return result, nil
}

func (q *Queries) InsertActivityRecords(ctx context.Context, records []ActivityRecordRow) (int, error) {
	batch := &pgx.Batch{}
	for i := range records {
		r := &records[i]
		batch.Queue(
			`INSERT INTO activity_records
			     (device_id, record_id, producer, app_name, title, url, started_at, ended_at, duration_s)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			 ON CONFLICT (device_id, record_id) DO NOTHING`,
			r.DeviceID, r.RecordID, string(r.Producer), r.AppName, r.Title, r.URL,
			r.StartedAt, r.EndedAt, r.DurationS,
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
	categoryTotals, err := q.getCategoryTotals(ctx, userID, records, deduplicator, start, end)
	if err != nil {
		return nil, err
	}
	summary := &ActivitySummary{
		Records:           effective,
		Totals:            deduplicator.totals(start, end),
		CategoryTotals:    categoryTotals,
		TimelineTruncated: sourceTruncated || effectiveTruncated,
		SourceTruncated:   sourceTruncated,
	}
	return summary, nil
}

func (q *Queries) getCategoryTotals(
	ctx context.Context,
	userID int64,
	records []ActivityRecordRow,
	deduplicator *activityDeduplicator,
	start, end time.Time,
) ([]CategoryTotalRow, error) {
	if len(records) == 0 {
		return nil, nil
	}
	appKeys := make([]string, 0, len(records))
	seenKeys := make(map[string]struct{}, len(records))
	for i := range records {
		key := CategoryAppKey(CanonicalAppName(&records[i]))
		if key == "" {
			continue
		}
		if _, exists := seenKeys[key]; !exists {
			seenKeys[key] = struct{}{}
			appKeys = append(appKeys, key)
		}
	}
	assignments, err := q.ListAppCategoryAssignments(ctx, userID, appKeys)
	if err != nil {
		return nil, err
	}
	hasAssignedOverride := false
	for i := range records {
		if records[i].CategoryOverridePresent && records[i].CategoryOverrideID != nil {
			hasAssignedOverride = true
			break
		}
	}
	if len(assignments) == 0 && !hasAssignedOverride {
		return nil, nil
	}
	categoryRows, err := q.listCategoryRows(ctx, userID)
	if err != nil {
		return nil, err
	}
	categories := make(map[int64]CategoryRow, len(categoryRows))
	for _, category := range categoryRows {
		categories[category.ID] = category
	}
	totals := deduplicator.categoryTotals(start, end, assignments, categories)
	if len(totals) == 1 && totals[0].CategoryID == nil {
		return nil, nil
	}
	return totals, nil
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
			`SELECT ar.id, ar.device_id, ar.record_id, ar.producer, ar.app_name, ar.title, ar.url, ar.started_at, ar.ended_at, ar.duration_s, ar.created_at,
			        o.activity_record_id IS NOT NULL, o.category_id
			 FROM activity_records ar
			 JOIN devices d ON d.id = ar.device_id
			 LEFT JOIN activity_record_category_overrides o
			   ON o.activity_record_id = ar.id AND o.user_id = d.user_id
			 WHERE d.user_id = $1 AND ar.started_at < $3 AND ar.ended_at > $2 AND ar.device_id = $4
			 ORDER BY ar.started_at, ar.device_id, ar.id
			 LIMIT $5`,
			userID, start, end, *deviceID, ActivitySourceLimit+1)
	} else {
		rows, err = q.pool.Query(ctx,
			`SELECT ar.id, ar.device_id, ar.record_id, ar.producer, ar.app_name, ar.title, ar.url, ar.started_at, ar.ended_at, ar.duration_s, ar.created_at,
			        o.activity_record_id IS NOT NULL, o.category_id
			 FROM activity_records ar
			 JOIN devices d ON d.id = ar.device_id
			 LEFT JOIN activity_record_category_overrides o
			   ON o.activity_record_id = ar.id AND o.user_id = d.user_id
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
		if err := scanActivityRecord(rows, &r); err != nil {
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

type activityRecordScanner interface {
	Scan(...any) error
}

func scanActivityRecord(row activityRecordScanner, record *ActivityRecordRow) error {
	return row.Scan(
		&record.ID,
		&record.DeviceID,
		&record.RecordID,
		&record.Producer,
		&record.AppName,
		&record.Title,
		&record.URL,
		&record.StartedAt,
		&record.EndedAt,
		&record.DurationS,
		&record.CreatedAt,
		&record.CategoryOverridePresent,
		&record.CategoryOverrideID,
	)
}

// ListEditableActivityRecords returns raw records for an application's detail
// page. It intentionally does not reuse the bounded dashboard timeline: every
// source record in the requested window remains reachable through keyset pages.
func (q *Queries) ListEditableActivityRecords(
	ctx context.Context,
	userID int64,
	filter *EditableActivityFilter,
) (*EditableActivityPage, error) {
	limit := filter.Limit
	if limit <= 0 || limit > EditableActivityRecordLimit {
		limit = EditableActivityRecordLimit
	}
	if !filter.Start.Before(filter.End) {
		return &EditableActivityPage{}, nil
	}

	subject, knownFamily := editableActivitySubject(filter.CanonicalAppName)
	args := []any{userID, filter.Start, filter.End}
	where := `d.user_id = $1 AND ar.started_at < $3 AND ar.ended_at > $2`
	if knownFamily {
		args = append(args, subject.producer, subject.browserProducers, subject.desktopKeys)
		where += fmt.Sprintf(
			` AND (ar.producer = $%d OR (ar.producer <> ALL($%d) AND lower(TRIM(regexp_replace(ar.app_name, '[[:space:]]+', ' ', 'g'))) = ANY($%d)))`,
			len(args)-2, len(args)-1, len(args),
		)
	} else {
		args = append(args, subject.browserProducers, subject.exactName)
		where += fmt.Sprintf(` AND ar.producer <> ALL($%d) AND ar.app_name = $%d`, len(args)-1, len(args))
	}
	if filter.DeviceID != nil {
		args = append(args, *filter.DeviceID)
		where += fmt.Sprintf(` AND ar.device_id = $%d`, len(args))
	}
	if filter.Before != nil {
		args = append(args, filter.Before.EndedAt, filter.Before.ID)
		where += fmt.Sprintf(` AND (ar.ended_at, ar.id) < ($%d, $%d)`, len(args)-1, len(args))
	}
	args = append(args, limit+1)

	// nosemgrep: gosec.G202-1 -- SQL contains only literals and placeholders.
	rows, err := q.pool.Query(ctx,
		`SELECT ar.id, ar.device_id, ar.record_id, ar.producer, ar.app_name, ar.title, ar.url, ar.started_at, ar.ended_at, ar.duration_s, ar.created_at,
		        o.activity_record_id IS NOT NULL, o.category_id
		 FROM activity_records ar
		 JOIN devices d ON d.id = ar.device_id
		 LEFT JOIN activity_record_category_overrides o
		   ON o.activity_record_id = ar.id AND o.user_id = d.user_id
		 WHERE `+where+`
		 ORDER BY ar.ended_at DESC, ar.id DESC
		 LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	page := &EditableActivityPage{}
	for rows.Next() {
		var record ActivityRecordRow
		if err := scanActivityRecord(rows, &record); err != nil {
			return nil, err
		}
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		last := page.Records[len(page.Records)-1]
		page.Next = &EditableActivityCursor{EndedAt: last.EndedAt, ID: last.ID}
	}
	return page, nil
}

// SetActivityRecordCategoryOverride stores an explicit category choice. A nil
// category means explicit Uncategorized; deleting the row restores inheritance.
func (q *Queries) SetActivityRecordCategoryOverride(
	ctx context.Context,
	userID, recordID int64,
	categoryID *int64,
) error {
	var storedID int64
	var err error
	if categoryID == nil {
		err = q.pool.QueryRow(ctx,
			`INSERT INTO activity_record_category_overrides (
			     activity_record_id, user_id, category_id, created_at, updated_at
			 )
			 SELECT ar.id, d.user_id, NULL, NOW(), NOW()
			 FROM activity_records ar
			 JOIN devices d ON d.id = ar.device_id
			 WHERE ar.id = $2 AND d.user_id = $1
			 ON CONFLICT (activity_record_id) DO UPDATE SET
			     user_id = EXCLUDED.user_id,
			     category_id = EXCLUDED.category_id,
			     updated_at = EXCLUDED.updated_at
			 RETURNING activity_record_id`, userID, recordID).Scan(&storedID)
	} else {
		err = q.pool.QueryRow(ctx,
			`INSERT INTO activity_record_category_overrides (
			     activity_record_id, user_id, category_id, created_at, updated_at
			 )
			 SELECT ar.id, d.user_id, c.id, NOW(), NOW()
			 FROM activity_records ar
			 JOIN devices d ON d.id = ar.device_id
			 JOIN categories c ON c.id = $3 AND c.user_id = d.user_id
			 WHERE ar.id = $2 AND d.user_id = $1
			 ON CONFLICT (activity_record_id) DO UPDATE SET
			     user_id = EXCLUDED.user_id,
			     category_id = EXCLUDED.category_id,
			     updated_at = EXCLUDED.updated_at
			 RETURNING activity_record_id`, userID, recordID, *categoryID).Scan(&storedID)
	}
	return err
}

// DeleteActivityRecordCategoryOverride removes an explicit choice so the
// record inherits its application's current default.
func (q *Queries) DeleteActivityRecordCategoryOverride(
	ctx context.Context,
	userID, recordID int64,
) error {
	var deletedID int64
	err := q.pool.QueryRow(ctx,
		`WITH owned AS (
		     SELECT ar.id
		     FROM activity_records ar
		     JOIN devices d ON d.id = ar.device_id
		     WHERE ar.id = $2 AND d.user_id = $1
		 ), deleted AS (
		     DELETE FROM activity_record_category_overrides o
		     USING owned
		     WHERE o.activity_record_id = owned.id
		 )
		 SELECT id FROM owned`, userID, recordID).Scan(&deletedID)
	return err
}

// SetActivityRecordApplicationCategory changes the selected record's
// application default and removes that record's override atomically.
func (q *Queries) SetActivityRecordApplicationCategory(
	ctx context.Context,
	userID, recordID int64,
	categoryID *int64,
) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning activity category transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var producer identity.Producer
	var appName string
	if err := tx.QueryRow(
		ctx,
		`SELECT ar.producer, ar.app_name
		 FROM activity_records ar
		 JOIN devices d ON d.id = ar.device_id
		 WHERE ar.id = $2 AND d.user_id = $1`, userID, recordID,
	).Scan(&producer, &appName); err != nil {
		return err
	}
	if err := setAppCategory(ctx, tx, userID, CategoryAppKey(canonicalAppName(producer, appName)), categoryID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM activity_record_category_overrides
		 WHERE activity_record_id = $1`, recordID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing activity category transaction: %w", err)
	}
	return nil
}

// SiteTotalLimit caps how many sites the summary lists.
const SiteTotalLimit = 25

// siteExpr derives a display host from a URL.
//
// It extracts the hostname specifically, not the URL authority. The
// authority also carries userinfo and a port, so grouping on it puts
// credentials on the dashboard -- an https URL whose authority opens with
// "user:password@" renders that password verbatim -- and splits one site
// across rows for example.com, example.com:443, and EXAMPLE.com.
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
