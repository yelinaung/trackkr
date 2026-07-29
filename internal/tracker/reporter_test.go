package tracker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

const (
	testFirefoxApp      = "Firefox"
	testRecordTitle     = "Test"
	testVSCodeApp       = "VS Code"
	testMainGoTitle     = "main.go"
	testUnusedServerURL = "http://unused"
	testValue           = "test"
)

func testReporterConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{
		ServerURL:     "", // set per test via httptest
		APIKey:        "test-api-key",
		FlushInterval: Duration{100 * time.Millisecond},
		FlushSize:     5,
		DataDir:       t.TempDir(),
	}
}

func TestEnqueueAndFlush(t *testing.T) {
	t.Parallel()
	var received ingestRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-api-key" {
			t.Errorf("API key = %q, want %q", r.Header.Get("X-API-Key"), "test-api-key")
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"accepted":2}`))
	}))
	defer srv.Close()

	cfg := testReporterConfig(t)
	cfg.ServerURL = srv.URL
	logger := zerolog.Nop()
	reporter := NewReporter(cfg, srv.Client(), &logger)

	now := time.Now().Truncate(time.Second)
	reporter.Enqueue(&Record{
		AppName:   testFirefoxApp,
		Title:     testRecordTitle,
		StartedAt: now,
		EndedAt:   now.Add(30 * time.Second),
		DurationS: 30,
	})
	reporter.Enqueue(&Record{
		AppName:   testVSCodeApp,
		Title:     testMainGoTitle,
		StartedAt: now.Add(30 * time.Second),
		EndedAt:   now.Add(60 * time.Second),
		DurationS: 30,
	})

	err := reporter.flush(context.Background())
	if err != nil {
		t.Fatalf("flush: %v", err)
	}

	if len(received.Records) != 2 {
		t.Errorf("received %d records, want 2", len(received.Records))
	}
	if reporter.QueueLen() != 0 {
		t.Errorf("queue len = %d after flush, want 0", reporter.QueueLen())
	}
}

func TestFlushNetworkFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := testReporterConfig(t)
	cfg.ServerURL = srv.URL
	logger := zerolog.Nop()
	reporter := NewReporter(cfg, srv.Client(), &logger)

	reporter.Enqueue(&Record{
		AppName:   testFirefoxApp,
		Title:     testRecordTitle,
		StartedAt: time.Now(),
		EndedAt:   time.Now().Add(time.Second),
		DurationS: 1,
	})

	err := reporter.flush(context.Background())
	if err == nil {
		t.Fatal("expected error on 500 response")
	}

	if reporter.QueueLen() != 1 {
		t.Errorf("queue len = %d after failed flush, want 1 (records requeued)", reporter.QueueLen())
	}
}

func TestFlushDropsServerAcknowledgedInvalidRecord(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"accepted":0,"rejected":1}`))
	}))
	defer srv.Close()

	cfg := testReporterConfig(t)
	cfg.ServerURL = srv.URL
	logger := zerolog.Nop()
	reporter := NewReporter(cfg, srv.Client(), &logger)
	now := time.Now()
	reporter.Enqueue(&Record{
		AppName:   testFirefoxApp,
		StartedAt: now,
		EndedAt:   now,
	})

	if err := reporter.flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if reporter.QueueLen() != 0 {
		t.Errorf("queue len = %d, want acknowledged invalid record removed", reporter.QueueLen())
	}
}

func TestFlushEmptyQueue(t *testing.T) {
	t.Parallel()
	cfg := testReporterConfig(t)
	cfg.ServerURL = testUnusedServerURL
	logger := zerolog.Nop()
	reporter := NewReporter(cfg, http.DefaultClient, &logger)

	err := reporter.flush(context.Background())
	if err != nil {
		t.Fatalf("flush empty: %v", err)
	}
}

func TestPendingPersistence(t *testing.T) {
	t.Parallel()
	cfg := testReporterConfig(t)
	cfg.ServerURL = "http://unreachable:1"
	logger := zerolog.Nop()

	reporter := NewReporter(cfg, http.DefaultClient, &logger)

	now := time.Now().Truncate(time.Second)
	reporter.Enqueue(&Record{
		AppName:   testFirefoxApp,
		Title:     testRecordTitle,
		StartedAt: now,
		EndedAt:   now.Add(30 * time.Second),
		DurationS: 30,
	})

	// Save pending.
	if err := reporter.savePending(); err != nil {
		t.Fatalf("savePending: %v", err)
	}

	pendingPath := filepath.Join(cfg.DataDir, "pending.json")
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("pending file not created: %v", err)
	}

	// Create new reporter — should load the pending records.
	reporter2 := NewReporter(cfg, http.DefaultClient, &logger)
	if reporter2.QueueLen() != 1 {
		t.Errorf("queue len after reload = %d, want 1", reporter2.QueueLen())
	}

	// Pending file should be removed after loading.
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Error("pending file should be removed after loading")
	}
}

func TestShutdownSavesPending(t *testing.T) {
	t.Parallel()
	// Server that always fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := testReporterConfig(t)
	cfg.ServerURL = srv.URL
	logger := zerolog.Nop()
	reporter := NewReporter(cfg, srv.Client(), &logger)

	reporter.Enqueue(&Record{
		AppName:   testFirefoxApp,
		Title:     testRecordTitle,
		StartedAt: time.Now(),
		EndedAt:   time.Now().Add(time.Second),
		DurationS: 1,
	})

	if err := reporter.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Pending file should exist with the record.
	pendingPath := filepath.Join(cfg.DataDir, "pending.json")
	data, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatalf("reading pending file: %v", err)
	}
	var records []Record
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatalf("parsing pending file: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("pending records = %d, want 1", len(records))
	}
}

func TestFlushOnThreshold(t *testing.T) {
	t.Parallel()
	requests := make(chan int, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ingestRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		requests <- len(req.Records)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"accepted":5}`))
	}))
	defer srv.Close()

	cfg := testReporterConfig(t)
	cfg.ServerURL = srv.URL
	cfg.FlushSize = 3
	cfg.FlushInterval = Duration{10 * time.Second} // Long interval so only threshold triggers.
	logger := zerolog.Nop()
	reporter := NewReporter(cfg, srv.Client(), &logger)

	go reporter.Run(t.Context())

	for range 3 {
		reporter.Enqueue(&Record{
			AppName:   testValue,
			Title:     testValue,
			StartedAt: time.Now(),
			EndedAt:   time.Now().Add(time.Second),
			DurationS: 1,
		})
	}

	select {
	case count := <-requests:
		if count != 3 {
			t.Errorf("flushed %d records, want 3", count)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for flush")
	}
}
