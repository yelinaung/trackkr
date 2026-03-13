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

type UserRow struct {
	ID        int64
	Username  string
	Password  string
	CreatedAt time.Time
}
