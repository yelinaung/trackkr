package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/db"
	"github.com/yelinaung/trackkr/internal/icon"
)

// Shared test fixtures. Extracted so goconst stays quiet and the values
// have one obvious home.
const (
	testHost          = "127.0.0.1"
	testSiteHost      = "example.com"
	testLoginPath     = "/login"
	testCSRFValue     = "tok"
	testLimiterIP     = "10.0.0.1"
	testPassword      = "correct horse battery"
	testBadPass       = "wrong"
	testLaptop        = "laptop"
	testDesktop       = "desktop"
	testAppCode       = "code"
	testFirefoxLower  = "firefox"
	testFinderLower   = "finder"
	testPasswordField = "password"
	testUsernameField = "username"
	testNewUser       = "newcomer"
	testGoodPassword  = "a-long-enough-password"
)

// mockQuerier implements Querier for unit tests without a database.
type mockQuerier struct {
	devices     map[string]*db.DeviceRow // keyed by API key
	inserted    int
	insertFn    func(ctx context.Context, records []db.ActivityRecordRow) (int, error)
	icons       []icon.App
	iconUserID  int64
	iconEvicted int
	iconErr     error
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

func (m *mockQuerier) ListDevicesByUser(_ context.Context, userID int64) ([]db.DeviceRow, error) {
	var out []db.DeviceRow
	for _, d := range m.devices {
		if d.UserID == userID {
			out = append(out, *d)
		}
	}
	return out, nil
}

func (m *mockQuerier) UpsertAppIcons(_ context.Context, userID int64, apps []icon.App) (int, error) {
	m.iconUserID = userID
	m.icons = append([]icon.App(nil), apps...)
	return m.iconEvicted, m.iconErr
}

// unitServer creates a Server backed by a mockQuerier (no DB needed).
// Only the api field is populated: these tests exercise /api/v1 routes,
// and leaving sessions and web nil keeps the fake at three methods
// instead of every query in the package.
func unitServer(t *testing.T) (*Server, *mockQuerier) {
	t.Helper()
	mock := newMockQuerier()
	cfg := &Config{
		Server: ServerConfig{Host: testHost, Port: 0},
	}
	logger := zerolog.Nop()

	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}

	srv := &Server{
		config:    cfg,
		router:    nil,
		api:       mock,
		iconWrite: mock,
		logger:    &logger,
		templates: tmpl,
		codec:     newSessionCodec(testSecret, true),
		limiter:   newAttemptLimiter(loginAttemptLimit, loginAttemptWindow),
		iconLimit: newSlidingWindowLimiter(appIconRateLimit, appIconRateWindow),
		loc:       time.UTC,
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
