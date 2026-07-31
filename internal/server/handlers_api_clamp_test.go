package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/db"
)

func TestClampSuspendedInterval(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 7, 31, 10, 44, 18, 0, time.UTC)

	for _, tc := range []struct {
		name      string
		endedAt   time.Time
		durationS int
		wantEnd   time.Time
		wantTrim  bool
	}{
		{
			// The record behind a two-hour "Chrome session" recorded across a
			// closed lid: 63 seconds measured, 7769 seconds of wall clock.
			name:      "suspended interval is trimmed to the measured duration",
			endedAt:   start.Add(7769 * time.Second),
			durationS: 63,
			wantEnd:   start.Add(63 * time.Second),
			wantTrim:  true,
		},
		{
			name:      "consistent record is untouched",
			endedAt:   start.Add(600 * time.Second),
			durationS: 600,
			wantEnd:   start.Add(600 * time.Second),
		},
		{
			// A producer truncates its duration to whole seconds while keeping
			// sub-second timestamps, so a fraction over is normal.
			name:      "sub-second remainder is within slack",
			endedAt:   start.Add(600*time.Second + 900*time.Millisecond),
			durationS: 600,
			wantEnd:   start.Add(600*time.Second + 900*time.Millisecond),
		},
		{
			name:      "an interval shorter than the duration is left alone",
			endedAt:   start.Add(30 * time.Second),
			durationS: 600,
			wantEnd:   start.Add(30 * time.Second),
		},
		{
			// Nothing better to believe than the timestamps.
			name:      "no reported duration means no clamp",
			endedAt:   start.Add(7769 * time.Second),
			durationS: 0,
			wantEnd:   start.Add(7769 * time.Second),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, trimmed := clampSuspendedInterval(start, tc.endedAt, tc.durationS)
			if trimmed != tc.wantTrim {
				t.Errorf("trimmed = %v, want %v", trimmed, tc.wantTrim)
			}
			if !got.Equal(tc.wantEnd) {
				t.Errorf("ended_at = %s, want %s", got, tc.wantEnd)
			}
			if got.Before(start) || got.Equal(start) {
				t.Error("clamping produced an interval the database constraint would reject")
			}
		})
	}
}

// The guard has to hold at the boundary, not only in the helper: a daemon
// predating the tracker fix keeps reporting suspended intervals, and the
// dashboard reads the interval rather than the duration.
func TestIngestActivityClampsSuspendedInterval(t *testing.T) {
	t.Parallel()
	srv, mock := unitServer(t)
	fix := createMockFixtures(t, mock)

	var stored []db.ActivityRecordRow
	mock.insertFn = func(_ context.Context, records []db.ActivityRecordRow) (int, error) {
		stored = records
		return len(records), nil
	}

	now := time.Now().Truncate(time.Second)
	body := ingestRequest{
		Records: stampIngestIdentity(t, []ingestRecord{{
			AppName:   testFirefoxApp,
			Title:     testWindowTitle,
			StartedAt: now,
			EndedAt:   now.Add(7769 * time.Second),
			DurationS: 63,
		}}),
	}

	b, _ := json.Marshal(body)
	req := newRequest(t, http.MethodPost, "/api/v1/activity", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", fix.APIKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	var resp ingestResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Accepted != 1 || resp.Clamped != 1 {
		t.Errorf("accepted = %d, clamped = %d; want 1 and 1", resp.Accepted, resp.Clamped)
	}

	if len(stored) != 1 {
		t.Fatalf("stored %d rows, want 1", len(stored))
	}
	if span := stored[0].EndedAt.Sub(stored[0].StartedAt); span != 63*time.Second {
		t.Errorf("stored interval = %s, want 63s", span)
	}
	if stored[0].DurationS != 63 {
		t.Errorf("stored duration = %ds, want the reported 63", stored[0].DurationS)
	}
}
