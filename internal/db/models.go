package db

import (
	"time"
)

type ActivityRecordRow struct {
	ID        int64
	DeviceID  int64
	AppName   string
	Title     string
	URL       *string
	StartedAt time.Time
	EndedAt   time.Time
	DurationS int
	CreatedAt time.Time
}

type DeviceRow struct {
	ID         int64
	UserID     int64
	Name       string
	DeviceType string
	APIKey     string
	CreatedAt  time.Time
}

// AppTotalRow is one row of the per-app time summary for a window.
type AppTotalRow struct {
	AppName string
	Seconds int64
}

// SiteTotalRow is one row of the per-site browsing summary.
type SiteTotalRow struct {
	Site    string
	Seconds int64
}

// AppIconRow is one user-owned application icon and its metadata.
type AppIconRow struct {
	ID         int64
	UserID     int64
	AppKey     string
	PNG        []byte
	SHA256     []byte
	Width      int
	Height     int
	CreatedAt  time.Time
	UpdatedAt  time.Time
	LastSeenAt time.Time
}

type UserRow struct {
	ID        int64
	Username  string
	Password  string
	CreatedAt time.Time
}
