package db

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Queries struct {
	pool *pgxpool.Pool
}

func NewQueries(pool *pgxpool.Pool) *Queries {
	return &Queries{pool: pool}
}

func (q *Queries) InsertActivityRecords(ctx context.Context, records []ActivityRecordRow) (int, error) {
	batch := &pgx.Batch{}
	for _, r := range records {
		batch.Queue(
			`INSERT INTO activity_records (device_id, app_name, title, url, started_at, ended_at, duration_s)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			r.DeviceID, r.AppName, r.Title, r.URL, r.StartedAt, r.EndedAt, r.DurationS,
		)
	}

	br := q.pool.SendBatch(ctx, batch)
	defer br.Close()

	accepted := 0
	for range records {
		if _, err := br.Exec(); err != nil {
			return accepted, err
		}
		accepted++
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

func (q *Queries) GetActivityRecords(ctx context.Context, start, end time.Time, deviceID *int64) ([]ActivityRecordRow, error) {
	var rows pgx.Rows
	var err error

	if deviceID != nil {
		rows, err = q.pool.Query(ctx,
			`SELECT id, device_id, app_name, title, url, started_at, ended_at, duration_s, created_at
			 FROM activity_records
			 WHERE started_at >= $1 AND started_at < $2 AND device_id = $3
			 ORDER BY started_at`,
			start, end, *deviceID)
	} else {
		rows, err = q.pool.Query(ctx,
			`SELECT id, device_id, app_name, title, url, started_at, ended_at, duration_s, created_at
			 FROM activity_records
			 WHERE started_at >= $1 AND started_at < $2
			 ORDER BY started_at`,
			start, end)
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
