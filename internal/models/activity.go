package models

import "time"

type ActivityRecord struct {
	ID        int64     `json:"id"`
	DeviceID  int64     `json:"device_id"`
	AppName   string    `json:"app_name"`
	Title     string    `json:"title"`
	URL       string    `json:"url,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	DurationS int       `json:"duration_s"`
	CreatedAt time.Time `json:"created_at"`
}

type Device struct {
	ID         int64     `json:"id"`
	UserID     int64     `json:"user_id"`
	Name       string    `json:"name"`
	DeviceType string    `json:"device_type"`
	APIKey     string    `json:"api_key,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type User struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Password  string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}
