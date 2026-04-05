package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/db"
)

// mockQuerier implements Querier for unit tests without a database.
type mockQuerier struct {
	devices  map[string]*db.DeviceRow // keyed by API key
	inserted int
	insertFn func(ctx context.Context, records []db.ActivityRecordRow) (int, error)
}

func newMockQuerier() *mockQuerier {
	return &mockQuerier{
		devices: make(map[string]*db.DeviceRow),
	}
}

func (m *mockQuerier) GetDeviceByAPIKey(_ context.Context, apiKey string) (*db.DeviceRow, error) {
	d, ok := m.devices[apiKey]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return d, nil
}

func (m *mockQuerier) InsertActivityRecords(ctx context.Context, records []db.ActivityRecordRow) (int, error) {
	if m.insertFn != nil {
		return m.insertFn(ctx, records)
	}
	m.inserted += len(records)
	return len(records), nil
}

func (m *mockQuerier) addDevice(apiKey string, device *db.DeviceRow) {
	m.devices[apiKey] = device
}

// unitServer creates a Server backed by a mockQuerier (no DB needed).
func unitServer(t *testing.T) (*Server, *mockQuerier) {
	t.Helper()
	mock := newMockQuerier()
	cfg := &Config{
		Server: ServerConfig{Host: "127.0.0.1", Port: 0},
	}
	logger := zerolog.Nop()
	srv := &Server{
		config:  cfg,
		router:  nil,
		queries: mock,
		logger:  &logger,
	}
	srv.router = newRouter(srv)
	return srv, mock
}

type testFixturesMock struct {
	Device *db.DeviceRow
	APIKey string
}

func createMockFixtures(t *testing.T, mock *mockQuerier) *testFixturesMock {
	t.Helper()
	apiKey := fmt.Sprintf("testkey_%d", time.Now().UnixNano())
	device := &db.DeviceRow{
		ID:         1,
		UserID:     1,
		Name:       "test-laptop",
		DeviceType: "desktop",
		APIKey:     apiKey,
		CreatedAt:  time.Now(),
	}
	mock.addDevice(apiKey, device)
	return &testFixturesMock{Device: device, APIKey: apiKey}
}
