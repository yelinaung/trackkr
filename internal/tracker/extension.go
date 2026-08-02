package tracker

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/yelinaung/trackkr/internal/identity"
)

const (
	// extensionAppName marks URL-bearing Firefox records. Dashboard queries
	// give these records precedence over overlapping desktop Firefox records.
	extensionAppName = "Firefox"

	// chromeAppName is the canonical macOS and dashboard display name for
	// Chrome. The Linux X11 detector reports "google-chrome"; both normalize
	// into the same browser family for deduplication.
	chromeAppName = "Google Chrome"

	// minRecordDuration drops sub-second flicks through the tab strip.
	minRecordDuration = time.Second

	// maxRecordDuration is the longest single tab segment accepted. A
	// suspended laptop wakes with one enormous span; it is clamped
	// rather than dropped, since discarding it would also throw away the
	// real browsing that preceded the sleep.
	maxRecordDuration = 12 * time.Hour

	extensionReadTimeout = 5 * time.Second
	// extensionBodyTimeout bounds a slow upload; ReadHeaderTimeout only
	// covers the headers.
	extensionBodyTimeout = 30 * time.Second
	// extensionShutdownGrace is how long an in-flight handler has to
	// finish enqueueing before connections are forced closed.
	extensionShutdownGrace = 10 * time.Second
	extensionMaxBody       = 1 << 20

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
	// RecordID is minted by the extension when a segment starts and carried
	// through its durable queue, so its own retries replay rather than
	// duplicate. An absent or non-canonical value is replaced downstream.
	RecordID  string    `json:"record_id,omitempty"`
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
}

type extensionRequest struct {
	Records []extensionRecord `json:"records"`
}

type statusResponse struct {
	OK       bool     `json:"ok"`
	Browsers []string `json:"browsers"`
}

// idleResponse tells the extension when the user stopped, not merely
// whether they have.
//
// A boolean would tie the end of a browser segment to how soon the
// extension happened to ask. A timestamp lets it close the segment at
// the moment activity stopped whenever it learns, so a slow or deferred
// poll costs latency in writing the record and never accuracy in what
// the record says.
type idleResponse struct {
	Idle       bool       `json:"idle"`
	IdleSince  *time.Time `json:"idle_since,omitempty"`
	ThresholdS int        `json:"threshold_s"`
}

// enqueuer is the slice of Reporter the listener needs.
type enqueuer interface {
	Enqueue(rec *Record)
}

// ExtensionServer accepts tab activity from the browser extension on
// loopback and feeds it into the reporter queue, which already handles
// batching, retry, and on-disk persistence.
type ExtensionServer struct {
	server    *http.Server
	listener  net.Listener
	reporter  enqueuer
	idle      IdleDetector
	threshold time.Duration
	token     string
	logger    *zerolog.Logger
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

	// Config validation accepts "localhost" by name, but the name is
	// resolved by the system: a doctored /etc/hosts or NSS setup could
	// point it at a LAN address. Check what was actually bound, so the
	// loopback-only invariant does not depend on name resolution.
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !tcp.IP.IsLoopback() {
		_ = ln.Close()
		return nil, fmt.Errorf("refusing to serve on non-loopback address %s", ln.Addr())
	}
	return ln, nil
}

// NewExtensionServer wraps an already-bound listener.
func NewExtensionServer(
	cfg *Config,
	ln net.Listener,
	reporter enqueuer,
	idle IdleDetector,
	logger *zerolog.Logger,
) *ExtensionServer {
	e := &ExtensionServer{
		listener:  ln,
		reporter:  reporter,
		idle:      idle,
		threshold: cfg.IdleThreshold.Duration,
		token:     cfg.ExtensionToken,
		logger:    logger,
	}

	mux := http.NewServeMux()
	// The route selects the producer and the canonical application name.
	// A caller-supplied name would let a Chrome build claim Firefox
	// coverage, and an optional JSON field would let an old daemon accept a
	// Chrome batch and silently store it as Firefox. An unknown browser
	// route 404s, which the extension treats as retryable.
	mux.HandleFunc("/extension/activity", e.activityHandler(identity.ProducerFirefox, extensionAppName))
	mux.HandleFunc("/extension/activity/chrome", e.activityHandler(identity.ProducerChrome, chromeAppName))
	mux.HandleFunc("/extension/status", e.handleStatus)
	mux.HandleFunc("/extension/idle", e.handleIdle)

	e.server = &http.Server{
		Addr:              cfg.ExtensionAddr,
		Handler:           mux,
		ReadHeaderTimeout: extensionReadTimeout,
		ReadTimeout:       extensionBodyTimeout,
	}
	return e
}

// Serve serves until ctx is cancelled, and does not return until every
// in-flight handler has finished.
//
// That wait is the point: Serve returns the moment Shutdown *begins*, so
// returning there would let the caller run Reporter.Shutdown while a
// request is still decoding and enqueueing. Records added after the
// final flush would live only in memory and vanish with the process.
func (e *ExtensionServer) Serve(ctx context.Context) error {
	if e.listener == nil {
		return errors.New("extension listener: no listener")
	}

	shutdown := make(chan error, 1)
	go func() {
		<-ctx.Done()
		// ctx is already cancelled here, so the shutdown deadline hangs
		// off WithoutCancel rather than a fresh Background.
		shutdownCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), extensionShutdownGrace,
		)
		defer cancel()
		shutdown <- e.server.Shutdown(shutdownCtx)
	}()

	e.logger.Info().Str("addr", e.Addr()).Msg("extension listener started")

	err := e.server.Serve(e.listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("extension listener: %w", err)
	}

	// Serve stopped because Shutdown started; wait for it to drain.
	if err := <-shutdown; err != nil {
		// The grace period expired with handlers still running. Close
		// the connections out from under them and say so, rather than
		// blocking the daemon's shutdown indefinitely.
		_ = e.server.Close()
		return fmt.Errorf("extension listener shutdown: %w", err)
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
	// A Chrome build requires "chrome" here and reports "daemon upgrade
	// required" otherwise, so a new extension against an old daemon fails
	// visibly instead of having its records stored as Firefox. Older Firefox
	// builds ignore the added field.
	_ = json.NewEncoder(w).Encode(statusResponse{
		OK:       true,
		Browsers: []string{string(identity.ProducerFirefox), string(identity.ProducerChrome)},
	})
}

// handleIdle reports when the user stopped, for a browser extension
// that cannot find out for itself.
//
// browser.idle on Linux reads the X screensaver counter, which XWayland
// maintains from the events XWayland receives, so native Wayland input
// never touches it. An extension trusting that counter keeps timing a
// tab for as long as its user is away.
func (e *ExtensionServer) handleIdle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if !e.authorized(w, r) {
		return
	}

	// A detector that measures nothing reports zero idle and no error,
	// which reads as a present user. Serving that would keep the
	// browser's segment open for as long as swayidle or xprintidle
	// stayed missing.
	if !idleUsable(e.idle) {
		http.Error(w, `{"error":"idle detection unavailable"}`, http.StatusServiceUnavailable)
		return
	}

	idleFor, err := e.idle.IdleTime(r.Context())
	if err != nil {
		// Never answer a broken detector with idle:false. The extension
		// would hold its segment open for as long as the detector stayed
		// broken, which is the overcounting this route exists to stop.
		// A 503 sends it back to its own idle source instead.
		e.logger.Warn().Err(err).Msg("idle detection failed, telling the extension to fall back")
		http.Error(w, `{"error":"idle detection unavailable"}`, http.StatusServiceUnavailable)
		return
	}

	resp := idleResponse{ThresholdS: int(e.threshold.Seconds())}
	if idleFor >= e.threshold {
		since := time.Now().Add(-idleFor)
		resp.Idle = true
		resp.IdleSince = &since
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (e *ExtensionServer) activityHandler(
	producer identity.Producer,
	appName string,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		e.handleActivity(w, r, producer, appName)
	}
}

func (e *ExtensionServer) handleActivity(
	w http.ResponseWriter,
	r *http.Request,
	producer identity.Producer,
	appName string,
) {
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

	// One Decode stops at the end of the first JSON value, so
	// `{"records":[…]}garbage` would report every record accepted while
	// silently ignoring the rest -- and the size limit would never see
	// the unread tail. Require the stream to end where the value does.
	var req extensionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, extensionMaxBody))
	if err := dec.Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		http.Error(w, `{"error":"unexpected data after request body"}`, http.StatusBadRequest)
		return
	}
	if len(req.Records) == 0 {
		http.Error(w, `{"error":"no records provided"}`, http.StatusBadRequest)
		return
	}

	accepted := 0
	for i := range req.Records {
		rec, ok := toRecord(&req.Records[i], producer, appName)
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
	if !originAllowed(r.Header.Values("Origin")) {
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
func toRecord(in *extensionRecord, producer identity.Producer, appName string) (*Record, bool) {
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

	// A browser-supplied ID is preserved so a retry from the extension's own
	// durable queue conflicts as a replay. Anything not canonical is dropped
	// rather than normalized, and ensureIdentity derives a stable one.
	recordID := ""
	if identity.Valid(in.RecordID) {
		recordID = in.RecordID
	}

	return &Record{
		RecordID:  recordID,
		Producer:  producer,
		AppName:   appName,
		Title:     in.Title,
		URL:       in.URL,
		StartedAt: in.StartedAt,
		EndedAt:   ended,
		DurationS: int(dur.Seconds()),
	}, true
}

// originAllowed accepts an absent Origin and otherwise requires exactly one
// extension origin.
//
// The previous prefix test accepted anything beginning "moz-extension://",
// including credentials, a port, a path, or a query -- all of which a browser
// never sends and an attacker would have to construct deliberately. Parsing
// and demanding an empty everything-else is the difference between "starts
// with the right scheme" and "is an extension origin".
func originAllowed(values []string) bool {
	if len(values) == 0 {
		return true
	}
	// Duplicate headers and comma-joined lists are never sent by a browser
	// for Origin; treating either as one value invites confusion attacks.
	if len(values) > 1 || strings.Contains(values[0], ",") {
		return false
	}

	origin := values[0]
	if origin == "" {
		return true
	}

	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch parsed.Scheme {
	case "moz-extension", "chrome-extension":
	default:
		return false
	}
	// "null" parses as an opaque value rather than a host, so Opaque must be
	// empty and the host must be a bare extension ID with no port.
	return parsed.Opaque == "" &&
		parsed.User == nil &&
		parsed.Hostname() != "" &&
		parsed.Port() == "" &&
		parsed.Host == parsed.Hostname() &&
		parsed.Path == "" &&
		parsed.RawQuery == "" &&
		parsed.Fragment == ""
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
