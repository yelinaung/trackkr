package tracker

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

const (
	// extensionAppName marks browser records. The window tracker reports
	// "firefox" from WM_CLASS, so the differing case keeps the two
	// observations visibly separate on the dashboard until Phase 6
	// deduplicates them.
	extensionAppName = "Firefox"

	// minRecordDuration drops sub-second flicks through the tab strip.
	minRecordDuration = time.Second

	// maxRecordDuration is the longest single tab segment accepted. A
	// suspended laptop wakes with one enormous span; it is clamped
	// rather than dropped, since discarding it would also throw away the
	// real browsing that preceded the sleep.
	maxRecordDuration = 12 * time.Hour

	extensionReadTimeout = 5 * time.Second
	extensionMaxBody     = 1 << 20

	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// GenerateExtensionToken returns a fresh token for the browser
// extension. It is printed by "trackkrd -print-extension-token" and
// pasted into both the config file and the extension's options page.
func GenerateExtensionToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// extensionRecord is one tab segment as the extension reports it. The
// daemon computes the duration itself rather than trusting the caller.
type extensionRecord struct {
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
}

type extensionRequest struct {
	Records []extensionRecord `json:"records"`
}

// enqueuer is the slice of Reporter the listener needs.
type enqueuer interface {
	Enqueue(rec *Record)
}

// ExtensionServer accepts tab activity from the browser extension on
// loopback and feeds it into the reporter queue, which already handles
// batching, retry, and on-disk persistence.
type ExtensionServer struct {
	server   *http.Server
	listener net.Listener
	reporter enqueuer
	token    string
	logger   *zerolog.Logger
}

// ListenExtension binds the configured address.
//
// Binding is deliberately separate from the server so a caller can do it
// before constructing anything that holds state. NewReporter loads
// pending.json and deletes it, so a bind failure after that point would
// discard records that were safely on disk a moment earlier.
func ListenExtension(ctx context.Context, addr string) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}
	return ln, nil
}

// NewExtensionServer wraps an already-bound listener.
func NewExtensionServer(
	cfg *Config,
	ln net.Listener,
	reporter enqueuer,
	logger *zerolog.Logger,
) *ExtensionServer {
	e := &ExtensionServer{
		listener: ln,
		reporter: reporter,
		token:    cfg.ExtensionToken,
		logger:   logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/extension/activity", e.handleActivity)
	mux.HandleFunc("/extension/status", e.handleStatus)

	e.server = &http.Server{
		Addr:              cfg.ExtensionAddr,
		Handler:           mux,
		ReadHeaderTimeout: extensionReadTimeout,
	}
	return e
}

// Serve serves until ctx is cancelled.
func (e *ExtensionServer) Serve(ctx context.Context) error {
	if e.listener == nil {
		return errors.New("extension listener: no listener")
	}

	go func() {
		<-ctx.Done()
		// ctx is already cancelled here, so the shutdown deadline hangs
		// off WithoutCancel rather than a fresh Background.
		shutdownCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), extensionReadTimeout,
		)
		defer cancel()
		_ = e.server.Shutdown(shutdownCtx)
	}()

	e.logger.Info().Str("addr", e.Addr()).Msg("extension listener started")

	if err := e.server.Serve(e.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("extension listener: %w", err)
	}
	return nil
}

// Addr reports the address actually bound, which differs from the
// configured one when the port is 0.
func (e *ExtensionServer) Addr() string {
	if e.listener == nil {
		return e.server.Addr
	}
	return e.listener.Addr().String()
}

func (e *ExtensionServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if !e.authorized(w, r) {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (e *ExtensionServer) handleActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Requiring JSON is a security control, not politeness: a form post
	// cannot set this content type, so any cross-origin caller needs a
	// preflight, which this server never answers.
	if !hasJSONContentType(r) {
		http.Error(w, `{"error":"expected application/json"}`, http.StatusUnsupportedMediaType)
		return
	}
	if !e.authorized(w, r) {
		return
	}

	var req extensionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, extensionMaxBody)).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if len(req.Records) == 0 {
		http.Error(w, `{"error":"no records provided"}`, http.StatusBadRequest)
		return
	}

	accepted := 0
	for i := range req.Records {
		rec, ok := toRecord(&req.Records[i])
		if !ok {
			continue
		}
		e.reporter.Enqueue(rec)
		accepted++
	}

	e.logger.Debug().
		Int("received", len(req.Records)).
		Int("accepted", accepted).
		Msg("extension records queued")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]int{"accepted": accepted})
}

// authorized enforces the bearer token and rejects web origins. Both
// matter: any local process can reach a loopback port, and any page the
// user visits can attempt a cross-origin write.
func (e *ExtensionServer) authorized(w http.ResponseWriter, r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" && !strings.HasPrefix(origin, "moz-extension://") {
		http.Error(w, `{"error":"forbidden origin"}`, http.StatusForbidden)
		return false
	}

	sent := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if e.token == "" || !hmac.Equal([]byte(sent), []byte(e.token)) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return false
	}
	return true
}

// hasJSONContentType parses the header so a charset parameter does not
// cause a spurious rejection.
func hasJSONContentType(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/json"
}

// toRecord validates and converts one reported segment. It reports false
// for anything that should never reach the server.
func toRecord(in *extensionRecord) (*Record, bool) {
	if !isWebURL(in.URL) {
		return nil, false
	}

	// A missing timestamp decodes to year 1, and the subtraction then
	// saturates past the cap, so an absent started_at would be clamped
	// into a plausible-looking 12-hour record beginning in year 1 and
	// inserted by a server that does no temporal validation of its own.
	if in.StartedAt.IsZero() || in.EndedAt.IsZero() {
		return nil, false
	}

	ended := in.EndedAt
	if d := ended.Sub(in.StartedAt); d > maxRecordDuration {
		ended = in.StartedAt.Add(maxRecordDuration)
	}

	dur := ended.Sub(in.StartedAt)
	if dur < minRecordDuration {
		return nil, false
	}

	return &Record{
		AppName:   extensionAppName,
		Title:     in.Title,
		URL:       in.URL,
		StartedAt: in.StartedAt,
		EndedAt:   ended,
		DurationS: int(dur.Seconds()),
	}, true
}

// isWebURL keeps about:, moz-extension:, and file: URLs out of the
// timeline entirely.
func isWebURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == schemeHTTP || u.Scheme == schemeHTTPS) && u.Host != ""
}
