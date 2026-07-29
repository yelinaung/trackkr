package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/db"
	"github.com/yelinaung/trackkr/internal/icon"
)

const (
	appIconBatchLimit    = 10
	appIconBodyLimit     = 1 << 20
	appIconRateLimit     = 60
	appIconRateWindow    = time.Hour
	rateLimitSweepPeriod = time.Hour
	jsonContentType      = "application/json"
	apiErrorKey          = "error"
)

type appIconWriter interface {
	UpsertAppIcons(context.Context, int64, []icon.App) (int, error)
}

type appIconReader interface {
	AppIconMetadata(context.Context, int64, []string) ([]db.AppIconRow, error)
	AppIcon(context.Context, int64, int64) (*db.AppIconRow, error)
}

type appIconRequest struct {
	Icons []icon.App `json:"icons"`
}

type appIconResponse struct {
	Accepted int `json:"accepted"`
}

type slidingWindowLimiter struct {
	mu        sync.Mutex
	hits      map[int64][]time.Time
	limit     int
	window    time.Duration
	lastSweep time.Time
}

func newSlidingWindowLimiter(limit int, window time.Duration) *slidingWindowLimiter {
	return &slidingWindowLimiter{
		hits:   make(map[int64][]time.Time),
		limit:  limit,
		window: window,
	}
}

// reserve atomically spends one request and returns the wait when full.
func (l *slidingWindowLimiter) reserve(key int64, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.lastSweep.IsZero() || now.Sub(l.lastSweep) >= rateLimitSweepPeriod {
		for id, hits := range l.hits {
			hits = activeIconHits(hits, now.Add(-l.window))
			if len(hits) == 0 {
				delete(l.hits, id)
				continue
			}
			l.hits[id] = hits
		}
		l.lastSweep = now
	}

	hits := activeIconHits(l.hits[key], now.Add(-l.window))
	if len(hits) >= l.limit {
		l.hits[key] = hits
		return false, max(hits[0].Add(l.window).Sub(now), time.Second)
	}
	l.hits[key] = append(hits, now)
	return true, 0
}

func activeIconHits(hits []time.Time, cutoff time.Time) []time.Time {
	first := 0
	for first < len(hits) && !hits[first].After(cutoff) {
		first++
	}
	return hits[first:]
}

func handleAppIconUpload(
	writer appIconWriter,
	limiter *slidingWindowLimiter,
	logger *zerolog.Logger,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		device := DeviceFromContext(r.Context())
		if device == nil {
			writeAPIError(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		allowed, retryAfter := limiter.reserve(device.ID, time.Now())
		if !allowed {
			seconds := (retryAfter + time.Second - 1) / time.Second
			w.Header().Set("Retry-After", strconv.FormatInt(int64(seconds), 10))
			writeAPIError(w, "app icon upload rate exceeded", http.StatusTooManyRequests)
			return
		}

		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != jsonContentType {
			writeAPIError(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, appIconBodyLimit)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()

		var req appIconRequest
		if err := decoder.Decode(&req); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeAPIError(w, "request body is too large", http.StatusRequestEntityTooLarge)
				return
			}
			writeAPIError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if err := rejectTrailingJSON(decoder); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeAPIError(w, "request body is too large", http.StatusRequestEntityTooLarge)
				return
			}
			writeAPIError(w, "invalid trailing request data", http.StatusBadRequest)
			return
		}
		if len(req.Icons) == 0 || len(req.Icons) > appIconBatchLimit {
			writeAPIError(w, "icons must contain 1 through 10 entries", http.StatusBadRequest)
			return
		}

		seen := make(map[string]struct{}, len(req.Icons))
		for i, appIcon := range req.Icons {
			if _, duplicate := seen[appIcon.Key]; duplicate {
				writeAPIError(w, "duplicate app icon key", http.StatusBadRequest)
				return
			}
			seen[appIcon.Key] = struct{}{}
			if _, err := icon.Validate(appIcon); err != nil {
				writeAPIError(w, fmt.Sprintf("invalid icon %d", i), http.StatusUnprocessableEntity)
				return
			}
		}

		evicted, err := writer.UpsertAppIcons(r.Context(), device.UserID, req.Icons)
		if err != nil {
			if logger != nil {
				logger.Error().Err(err).Int64("user_id", device.UserID).Msg("storing application icons")
			}
			writeAPIError(w, "failed to store app icons", http.StatusInternalServerError)
			return
		}
		if evicted > 0 && logger != nil {
			logger.Warn().
				Int64("user_id", device.UserID).
				Int("evicted", evicted).
				Msg("pruned application icons above user limit")
		}

		w.Header().Set("Content-Type", jsonContentType)
		_ = json.NewEncoder(w).Encode(appIconResponse{Accepted: len(req.Icons)})
	}
}

func rejectTrailingJSON(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("second JSON value")
	}
	return err
}

func writeAPIError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{apiErrorKey: message})
}

func (h *webHandlers) handleAppIcon() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil {
			http.NotFound(w, r)
			return
		}

		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil || id < 1 {
			http.NotFound(w, r)
			return
		}
		digest := chi.URLParam(r, "sha256")
		if !validDigestPath(digest) {
			http.NotFound(w, r)
			return
		}

		row, err := h.icons.AppIcon(r.Context(), user.ID, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.NotFound(w, r)
				return
			}
			h.fail(w, err, "loading application icon")
			return
		}
		if hex.EncodeToString(row.SHA256) != digest {
			http.NotFound(w, r)
			return
		}

		etag := `"` + digest + `"`
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		w.Header().Set("ETag", etag)
		w.Header().Add("Vary", "Cookie")
		if matchesETag(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(row.PNG)
	}
}

func validDigestPath(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func matchesETag(header, want string) bool {
	for part := range strings.SplitSeq(header, ",") {
		if tag := strings.TrimSpace(part); tag == want || tag == "*" {
			return true
		}
	}
	return false
}
