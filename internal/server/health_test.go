package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type testPinger struct {
	err   error
	calls int
}

func (p *testPinger) Ping(context.Context) error {
	p.calls++
	return p.err
}

func TestHealthzDoesNotUseDatabase(t *testing.T) {
	t.Parallel()

	srv, _ := unitServer(t)
	pinger := &testPinger{err: errors.New("database unavailable")}
	srv.database = pinger

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if pinger.calls != 0 {
		t.Errorf("database ping calls = %d, want 0", pinger.calls)
	}
}

func TestReadyz(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		pinger     *testPinger
		wantStatus int
	}{
		{name: "database ready", pinger: &testPinger{}, wantStatus: http.StatusOK},
		{
			name:       "database unavailable",
			pinger:     &testPinger{err: errors.New("database unavailable")},
			wantStatus: http.StatusServiceUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, _ := unitServer(t)
			srv.database = tt.pinger
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.pinger.calls != 1 {
				t.Errorf("database ping calls = %d, want 1", tt.pinger.calls)
			}
		})
	}
}
