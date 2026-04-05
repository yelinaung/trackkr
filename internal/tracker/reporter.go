package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// Record is the client-side representation of an activity record
// before it is sent to the server.
type Record struct {
	AppName   string    `json:"app_name"`
	Title     string    `json:"title"`
	URL       string    `json:"url,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	DurationS int       `json:"duration_s"`
}

type ingestRequest struct {
	Records []Record `json:"records"`
}

// HTTPPoster is the interface for sending HTTP requests.
// *http.Client satisfies this.
type HTTPPoster interface {
	Do(req *http.Request) (*http.Response, error)
}

// Reporter batches records and sends them to the server.
type Reporter struct {
	cfg     *Config
	client  HTTPPoster
	logger  *zerolog.Logger
	mu      sync.Mutex
	queue   []Record
	flushCh chan struct{}
	pending string
}

// NewReporter creates a reporter and loads any pending records from
// disk.
func NewReporter(cfg *Config, client HTTPPoster, logger *zerolog.Logger) *Reporter {
	r := &Reporter{
		cfg:     cfg,
		client:  client,
		logger:  logger,
		flushCh: make(chan struct{}, 1),
		pending: filepath.Join(cfg.DataDir, "pending.json"),
	}
	if err := r.loadPending(); err != nil {
		logger.Warn().Err(err).Msg("could not load pending records")
	}
	return r
}

// Enqueue adds a record to the queue. If the queue reaches
// FlushSize, it signals a flush.
func (r *Reporter) Enqueue(rec *Record) {
	r.mu.Lock()
	r.queue = append(r.queue, *rec)
	n := len(r.queue)
	r.mu.Unlock()

	if n >= r.cfg.FlushSize {
		select {
		case r.flushCh <- struct{}{}:
		default:
		}
	}
}

// Run starts the flush loop. It blocks until ctx is cancelled.
func (r *Reporter) Run(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.FlushInterval.Duration)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tryFlush(ctx)
		case <-r.flushCh:
			r.tryFlush(ctx)
		}
	}
}

// Shutdown performs a final flush and persists any remaining records.
func (r *Reporter) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r.tryFlush(ctx)

	r.mu.Lock()
	remaining := len(r.queue)
	r.mu.Unlock()

	if remaining > 0 {
		if err := r.savePending(); err != nil {
			return fmt.Errorf("saving pending records: %w", err)
		}
		r.logger.Info().Int("count", remaining).Msg("saved pending records to disk")
	}
	return nil
}

// QueueLen returns the current queue length (for testing).
func (r *Reporter) QueueLen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.queue)
}

func (r *Reporter) tryFlush(ctx context.Context) {
	if err := r.flush(ctx); err != nil {
		r.logger.Error().Err(err).Msg("flush failed")
	}
}

func (r *Reporter) flush(ctx context.Context) error {
	r.mu.Lock()
	if len(r.queue) == 0 {
		r.mu.Unlock()
		return nil
	}
	batch := make([]Record, len(r.queue))
	copy(batch, r.queue)
	r.queue = r.queue[:0]
	r.mu.Unlock()

	body, err := json.Marshal(ingestRequest{Records: batch})
	if err != nil {
		r.requeue(batch)
		return fmt.Errorf("marshaling records: %w", err)
	}

	url := r.cfg.ServerURL + "/api/v1/activity"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		r.requeue(batch)
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", r.cfg.APIKey)

	resp, err := r.client.Do(req)
	if err != nil {
		r.requeue(batch)
		return fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		r.requeue(batch)
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}

	r.logger.Debug().Int("count", len(batch)).Msg("flushed records")
	return nil
}

func (r *Reporter) requeue(records []Record) {
	r.mu.Lock()
	r.queue = append(records, r.queue...)
	r.mu.Unlock()
}

func (r *Reporter) loadPending() error {
	data, err := os.ReadFile(r.pending)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading pending file: %w", err)
	}

	var records []Record
	if err := json.Unmarshal(data, &records); err != nil {
		return fmt.Errorf("parsing pending file: %w", err)
	}

	if len(records) > 0 {
		r.mu.Lock()
		r.queue = append(records, r.queue...)
		r.mu.Unlock()
		r.logger.Info().Int("count", len(records)).Msg("loaded pending records")

		// Remove the file after successful load.
		_ = os.Remove(r.pending)
	}
	return nil
}

func (r *Reporter) savePending() error {
	r.mu.Lock()
	records := make([]Record, len(r.queue))
	copy(records, r.queue)
	r.mu.Unlock()

	if len(records) == 0 {
		return nil
	}

	data, err := json.Marshal(records)
	if err != nil {
		return fmt.Errorf("marshaling pending records: %w", err)
	}

	dir := filepath.Dir(r.pending)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}

	// Atomic write: temp file + rename.
	tmp := r.pending + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := os.Rename(tmp, r.pending); err != nil {
		return fmt.Errorf("renaming temp file: %w", err)
	}
	return nil
}
