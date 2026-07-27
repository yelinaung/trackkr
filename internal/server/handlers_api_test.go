package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/db"
)

const (
	testFirefoxApp = "Firefox"
	testPageTitle  = "Test Page"
	testPageURL    = "https://example.com"
)

func newRequest(t *testing.T, method, target string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestIngestActivity(t *testing.T) {
	t.Parallel()
	srv, mock := unitServer(t)
	fix := createMockFixtures(t, mock)

	now := time.Now().Truncate(time.Second)
	body := ingestRequest{
		Records: []ingestRecord{
			{
				AppName:   testFirefoxApp,
				Title:     testPageTitle,
				URL:       testPageURL,
				StartedAt: now,
				EndedAt:   now.Add(30 * time.Second),
				DurationS: 30,
			},
			{
				AppName:   "VS Code",
				Title:     "main.go",
				StartedAt: now.Add(30 * time.Second),
				EndedAt:   now.Add(60 * time.Second),
				DurationS: 30,
			},
		},
	}

	b, _ := json.Marshal(body)
	req := newRequest(t, http.MethodPost, "/api/v1/activity", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", fix.APIKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var resp ingestResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Accepted != 2 {
		t.Errorf("accepted = %d, want 2", resp.Accepted)
	}
	if mock.inserted != 2 {
		t.Errorf("mock.inserted = %d, want 2", mock.inserted)
	}
}

func TestIngestActivityNoAPIKey(t *testing.T) {
	t.Parallel()
	srv, _ := unitServer(t)

	req := newRequest(t, http.MethodPost, "/api/v1/activity", bytes.NewReader([]byte(`{"records":[]}`)))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestIngestActivityInvalidAPIKey(t *testing.T) {
	t.Parallel()
	srv, _ := unitServer(t)

	req := newRequest(t, http.MethodPost, "/api/v1/activity", bytes.NewReader([]byte(`{"records":[]}`)))
	req.Header.Set("X-API-Key", "invalid_key_xyz")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestIngestActivityEmptyRecords(t *testing.T) {
	t.Parallel()
	srv, mock := unitServer(t)
	fix := createMockFixtures(t, mock)

	req := newRequest(t, http.MethodPost, "/api/v1/activity", bytes.NewReader([]byte(`{"records":[]}`)))
	req.Header.Set("X-API-Key", fix.APIKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestIngestActivityInvalidJSON(t *testing.T) {
	t.Parallel()
	srv, mock := unitServer(t)
	fix := createMockFixtures(t, mock)

	req := newRequest(t, http.MethodPost, "/api/v1/activity", bytes.NewReader([]byte(`not json`)))
	req.Header.Set("X-API-Key", fix.APIKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestIngestActivityInsertError(t *testing.T) {
	t.Parallel()
	srv, mock := unitServer(t)
	fix := createMockFixtures(t, mock)

	mock.insertFn = func(_ context.Context, _ []db.ActivityRecordRow) (int, error) {
		return 0, errors.New("db connection lost")
	}

	now := time.Now().Truncate(time.Second)
	body := ingestRequest{
		Records: []ingestRecord{
			{
				AppName:   testFirefoxApp,
				Title:     testPageTitle,
				StartedAt: now,
				EndedAt:   now.Add(30 * time.Second),
				DurationS: 30,
			},
		},
	}

	b, _ := json.Marshal(body)
	req := newRequest(t, http.MethodPost, "/api/v1/activity", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", fix.APIKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestIngestActivityURLOptional(t *testing.T) {
	t.Parallel()
	srv, mock := unitServer(t)
	fix := createMockFixtures(t, mock)

	var captured []db.ActivityRecordRow
	mock.insertFn = func(_ context.Context, records []db.ActivityRecordRow) (int, error) {
		captured = records
		return len(records), nil
	}

	now := time.Now().Truncate(time.Second)
	body := ingestRequest{
		Records: []ingestRecord{
			{
				AppName:   "VS Code",
				Title:     "main.go",
				StartedAt: now,
				EndedAt:   now.Add(30 * time.Second),
				DurationS: 30,
			},
			{
				AppName:   testFirefoxApp,
				Title:     testPageTitle,
				URL:       testPageURL,
				StartedAt: now.Add(30 * time.Second),
				EndedAt:   now.Add(60 * time.Second),
				DurationS: 30,
			},
		},
	}

	b, _ := json.Marshal(body)
	req := newRequest(t, http.MethodPost, "/api/v1/activity", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", fix.APIKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if len(captured) != 2 {
		t.Fatalf("captured %d records, want 2", len(captured))
	}
	if captured[0].URL != nil {
		t.Errorf("record[0].URL = %v, want nil", captured[0].URL)
	}
	if captured[1].URL == nil || *captured[1].URL != testPageURL {
		t.Errorf("record[1].URL = %v, want %q", captured[1].URL, testPageURL)
	}
}

func TestHeartbeat(t *testing.T) {
	t.Parallel()
	srv, mock := unitServer(t)
	fix := createMockFixtures(t, mock)

	req := newRequest(t, http.MethodPost, "/api/v1/heartbeat", nil)
	req.Header.Set("X-API-Key", fix.APIKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %q, want %q", resp["status"], "ok")
	}
}

func TestHeartbeatNoAuth(t *testing.T) {
	t.Parallel()
	srv, _ := unitServer(t)

	req := newRequest(t, http.MethodPost, "/api/v1/heartbeat", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestUnknownRoute(t *testing.T) {
	t.Parallel()
	srv, _ := unitServer(t)

	req := newRequest(t, http.MethodGet, "/nonexistent", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestIngestActivityWrongMethod(t *testing.T) {
	t.Parallel()
	srv, mock := unitServer(t)
	fix := createMockFixtures(t, mock)

	req := newRequest(t, http.MethodGet, "/api/v1/activity", nil)
	req.Header.Set("X-API-Key", fix.APIKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// DeviceRow carries the plaintext API key, so marshalling it directly
// would let one compromised device key harvest every other key on the
// account. The response must be a DTO.
func TestListDevicesNeverExposesAPIKeys(t *testing.T) {
	t.Parallel()
	srv, mock := unitServer(t)
	fix := createMockFixtures(t, mock)

	req := newRequest(t, http.MethodGet, "/api/v1/devices", nil)
	req.Header.Set("X-API-Key", fix.APIKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, fix.APIKey) {
		t.Errorf("response leaked an API key: %s", body)
	}
	if strings.Contains(body, "api_key") {
		t.Errorf("response has an api_key field: %s", body)
	}

	var got devicesResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got.Devices) != 1 || got.Devices[0].Name != fix.Device.Name {
		t.Errorf("devices = %+v, want the caller's device", got.Devices)
	}
}

func TestListDevicesRequiresAPIKey(t *testing.T) {
	t.Parallel()
	srv, _ := unitServer(t)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newRequest(t, http.MethodGet, "/api/v1/devices", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
