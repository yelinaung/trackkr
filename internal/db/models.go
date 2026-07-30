package db

import (
	"time"

	"github.com/yelinaung/trackkr/internal/identity"
)

type ActivityRecordRow struct {
	ID        int64
	DeviceID  int64
	RecordID  string
	Producer  identity.Producer
	AppName   string
	Title     string
	URL       *string
	StartedAt time.Time
	EndedAt   time.Time
	DurationS int
	CreatedAt time.Time
}

// ActivitySummary is the bounded activity view used by one dashboard render.
type ActivitySummary struct {
	Records           []ActivityRecordRow
	Totals            []AppTotalRow
	TimelineTruncated bool
	SourceTruncated   bool
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

// SiteIconRow is one user-scoped favicon cache entry. A nil PNG records a
// failed attempt; ExpiresAt distinguishes definitive absence from retryable
// failures.
type SiteIconRow struct {
	ID          int64
	UserID      int64
	Site        string
	PNG         []byte
	SHA256      []byte
	Width       *int
	Height      *int
	AttemptedAt *time.Time
	ExpiresAt   time.Time
	ClaimUntil  *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type UserRow struct {
	ID        int64
	Username  string
	Password  string
	CreatedAt time.Time
}
