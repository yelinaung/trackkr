package tracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/icon"
)

const (
	appIconQueueLimit           = 128
	appIconFlushLimit           = 10
	appIconHTTPBaseTimeout      = 5 * time.Second
	appIconHTTPMaxTimeout       = 25 * time.Second
	appIconUploadBytesPerSecond = 64 << 10
	appIconShutdownTimeout      = 5 * time.Second
	appIconRetryMin             = time.Minute
	appIconRetryMax             = 15 * time.Minute
	appIconRetryAfterMax        = 24 * time.Hour
	activityAPIPath             = "/api/v1/activity"
	appIconAPIPath              = "/api/v1/app-icons"
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

type appIconUploadRequest struct {
	Icons []icon.App `json:"icons"`
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
	icons   map[string]icon.App
	flushCh chan struct{}
	pending string
	now     func() time.Time

	iconRetryAt  time.Time
	iconFailures int
}

// NewReporter creates a reporter and loads any pending records from
// disk.
func NewReporter(cfg *Config, client HTTPPoster, logger *zerolog.Logger) *Reporter {
	r := &Reporter{
		cfg:     cfg,
		client:  client,
		logger:  logger,
		icons:   make(map[string]icon.App),
		flushCh: make(chan struct{}, 1),
		pending: filepath.Join(cfg.DataDir, "pending.json"),
		now:     time.Now,
	}
	if err := r.loadPending(); err != nil {
		logger.Warn().Err(err).Msg("could not load pending records")
	}
	return r
}

// EnqueueAppIcon adds reproducible presentation metadata to the in-memory
// queue. It returns false when the value is invalid or a new key cannot fit.
func (r *Reporter) EnqueueAppIcon(appIcon icon.App) bool {
	if _, err := icon.Validate(appIcon); err != nil {
		return false
	}
	owned := icon.Clone(appIcon)

	r.mu.Lock()
	if _, exists := r.icons[owned.Key]; !exists && len(r.icons) >= appIconQueueLimit {
		r.mu.Unlock()
		return false
	}
	r.icons[owned.Key] = owned
	r.mu.Unlock()

	select {
	case r.flushCh <- struct{}{}:
	default:
	}
	return true
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

	r.tryFlushActivity(ctx)

	r.mu.Lock()
	remaining := len(r.queue)
	r.mu.Unlock()

	var shutdownErr error
	if remaining > 0 {
		if err := r.savePending(); err != nil {
			shutdownErr = fmt.Errorf("saving pending records: %w", err)
		} else {
			r.logger.Info().Int("count", remaining).Msg("saved pending records to disk")
		}
	}

	iconCtx, iconCancel := context.WithTimeout(context.Background(), appIconShutdownTimeout)
	defer iconCancel()
	r.tryFlushAppIconsNow(iconCtx)
	return shutdownErr
}

// QueueLen returns the current queue length (for testing).
func (r *Reporter) QueueLen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.queue)
}

// AppIconQueueLen returns the pending app-icon count for tests.
func (r *Reporter) AppIconQueueLen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.icons)
}

func (r *Reporter) tryFlush(ctx context.Context) {
	r.tryFlushActivity(ctx)
	r.tryFlushAppIcons(ctx)
}

func (r *Reporter) tryFlushActivity(ctx context.Context) {
	if err := r.flush(ctx); err != nil {
		r.logger.Error().Err(err).Msg("activity flush failed")
	}
}

func (r *Reporter) tryFlushAppIcons(ctx context.Context) {
	if err := r.flushAppIcons(ctx); err != nil {
		r.logger.Error().Err(err).Msg("application icon flush failed")
	}
}

func (r *Reporter) tryFlushAppIconsNow(ctx context.Context) {
	if err := r.flushAppIconsNow(ctx); err != nil {
		r.logger.Error().Err(err).Msg("application icon flush failed")
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

	url := r.cfg.ServerURL + activityAPIPath
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

func (r *Reporter) flushAppIcons(ctx context.Context) error {
	return r.flushAppIconsWithBackoff(ctx, false)
}

func (r *Reporter) flushAppIconsNow(ctx context.Context) error {
	return r.flushAppIconsWithBackoff(ctx, true)
}

func (r *Reporter) flushAppIconsWithBackoff(ctx context.Context, ignoreBackoff bool) error {
	now := r.now()
	r.mu.Lock()
	if len(r.icons) == 0 || (!ignoreBackoff && now.Before(r.iconRetryAt)) {
		r.mu.Unlock()
		return nil
	}
	keys := slices.Sorted(maps.Keys(r.icons))
	keys = keys[:min(len(keys), appIconFlushLimit)]
	batch := make([]icon.App, 0, len(keys))
	for _, key := range keys {
		batch = append(batch, icon.Clone(r.icons[key]))
	}
	r.mu.Unlock()

	flushCtx, cancel := context.WithTimeout(ctx, appIconHTTPMaxTimeout)
	defer cancel()
	result, err := r.postAppIcons(flushCtx, batch)
	if err != nil {
		r.deferAppIconRetry(0)
		return err
	}

	switch {
	case result.status == http.StatusOK:
		r.removeMatchingAppIcons(batch)
		r.clearAppIconRetry()
		r.logger.Debug().Int("count", len(batch)).Msg("flushed application icons")
		return nil
	case isPermanentAppIconStatus(result.status) && len(batch) == 1:
		r.removeMatchingAppIcons(batch)
		r.clearAppIconRetry()
		return fmt.Errorf("server permanently rejected an application icon with %d", result.status)
	case isPermanentAppIconStatus(result.status):
		return r.isolateRejectedAppIcons(flushCtx, batch, result.status)
	default:
		r.deferAppIconRetry(result.retryAfter)
		return fmt.Errorf("server returned %d for application icons", result.status)
	}
}

type appIconPostResult struct {
	status     int
	retryAfter time.Duration
}

func (r *Reporter) postAppIcons(ctx context.Context, batch []icon.App) (appIconPostResult, error) {
	body, err := json.Marshal(appIconUploadRequest{Icons: batch})
	if err != nil {
		return appIconPostResult{}, fmt.Errorf("marshaling application icons: %w", err)
	}

	requestCtx, cancel := context.WithTimeout(ctx, appIconRequestTimeout(len(body)))
	defer cancel()
	req, err := http.NewRequestWithContext(
		requestCtx,
		http.MethodPost,
		r.cfg.ServerURL+appIconAPIPath,
		bytes.NewReader(body),
	)
	if err != nil {
		return appIconPostResult{}, fmt.Errorf("creating application icon request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", r.cfg.APIKey)

	resp, err := r.client.Do(req)
	if err != nil {
		return appIconPostResult{}, fmt.Errorf("sending application icons: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	return appIconPostResult{
		status:     resp.StatusCode,
		retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), r.now()),
	}, nil
}

func (r *Reporter) isolateRejectedAppIcons(
	ctx context.Context,
	batch []icon.App,
	batchStatus int,
) error {
	permanentlyRejected := 0
	for _, appIcon := range batch {
		result, err := r.postAppIcons(ctx, []icon.App{appIcon})
		if err != nil {
			r.deferAppIconRetry(0)
			return fmt.Errorf("isolating a rejected application icon: %w", err)
		}

		switch {
		case result.status == http.StatusOK:
			r.removeMatchingAppIcons([]icon.App{appIcon})
		case isPermanentAppIconStatus(result.status):
			r.removeMatchingAppIcons([]icon.App{appIcon})
			permanentlyRejected++
		default:
			r.deferAppIconRetry(result.retryAfter)
			return fmt.Errorf("server returned %d while isolating application icons", result.status)
		}
	}

	r.clearAppIconRetry()
	if permanentlyRejected > 0 {
		return fmt.Errorf(
			"server permanently rejected %d application icon(s) after batch status %d",
			permanentlyRejected,
			batchStatus,
		)
	}
	return nil
}

func isPermanentAppIconStatus(status int) bool {
	return status == http.StatusBadRequest ||
		status == http.StatusRequestEntityTooLarge ||
		status == http.StatusUnprocessableEntity
}

func appIconRequestTimeout(bodyBytes int) time.Duration {
	transferTime := time.Duration(bodyBytes) * time.Second / appIconUploadBytesPerSecond
	return min(appIconHTTPBaseTimeout+transferTime, appIconHTTPMaxTimeout)
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		seconds = min(seconds, int64(appIconRetryAfterMax/time.Second))
		return time.Duration(seconds) * time.Second
	}
	if retryAt, err := http.ParseTime(value); err == nil {
		return min(max(retryAt.Sub(now), 0), appIconRetryAfterMax)
	}
	return 0
}

func (r *Reporter) deferAppIconRetry(retryAfter time.Duration) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()

	r.iconFailures++
	backoff := appIconRetryMin
	for range min(r.iconFailures-1, 4) {
		backoff *= 2
	}
	backoff = min(backoff, appIconRetryMax)
	r.iconRetryAt = now.Add(max(backoff, retryAfter))
}

func (r *Reporter) clearAppIconRetry() {
	r.mu.Lock()
	r.iconRetryAt = time.Time{}
	r.iconFailures = 0
	r.mu.Unlock()
}

func (r *Reporter) removeMatchingAppIcons(batch []icon.App) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, uploaded := range batch {
		current, ok := r.icons[uploaded.Key]
		if !ok || sha256.Sum256(current.PNG) != sha256.Sum256(uploaded.PNG) {
			continue
		}
		// A just-queued identical value can go too: the server now has
		// exactly that digest. A different replacement remains queued.
		delete(r.icons, uploaded.Key)
	}
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
