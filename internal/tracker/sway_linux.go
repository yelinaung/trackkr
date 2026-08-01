//go:build linux

package tracker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// Sway speaks the i3 IPC protocol: the magic string, then the payload
// length and the message type in the host's byte order, then a JSON
// payload. Lengths are native-endian because the socket never leaves
// the machine.
const (
	swayMagic      = "i3-ipc"
	swayHeaderLen  = len(swayMagic) + 8
	swayMsgGetTree = 4

	// swayMsgGetVersion is what discovery probes a candidate with. It
	// is cheap, read-only, and no other service listening at a matching
	// name will answer it in sway's shape.
	swayMsgGetVersion = 7

	// swayMaxPayload bounds an allocation driven by bytes off the wire.
	// A tree for a session with hundreds of windows is a few hundred
	// kilobytes, so anything past this is a desynced stream.
	swayMaxPayload = 32 << 20

	swayDialTimeout = 2 * time.Second
	swayIOTimeout   = 2 * time.Second
)

// Node types sway uses for windows. Everything above them -- root,
// output, workspace -- is layout, not something the user works in.
const (
	swayNodeCon         = "con"
	swayNodeFloatingCon = "floating_con"
)

var (
	// ErrNoSwaySocket reports that no sway IPC socket was advertised in
	// the environment, which is how the daemon tells a sway session from
	// an X11 one.
	ErrNoSwaySocket = errors.New("SWAYSOCK is not set")

	errSwayBadMagic = errors.New("sway IPC reply has a bad magic string")
)

// swayNode is the subset of a sway tree node the tracker reads. app_id
// is null for XWayland clients, which carry WM_CLASS in
// window_properties instead.
type swayNode struct {
	Type             string     `json:"type"`
	Name             string     `json:"name"`
	AppID            string     `json:"app_id"`
	Focused          bool       `json:"focused"`
	Nodes            []swayNode `json:"nodes"`
	FloatingNodes    []swayNode `json:"floating_nodes"`
	WindowProperties struct {
		Class string `json:"class"`
	} `json:"window_properties"`
}

// SwayWindowDetector reads the focused window from sway's IPC socket.
//
// It talks the protocol directly rather than shelling out to swaymsg:
// the tracker polls every few seconds for the life of the session, and
// a held connection costs nothing next to a process spawn per poll.
type SwayWindowDetector struct {
	logger *zerolog.Logger

	// socketPath is mutable because a restarted compositor usually
	// listens somewhere new. See rediscoverLocked.
	mu         sync.Mutex
	socketPath string
	conn       net.Conn
}

// NewSwayWindowDetector connects to the compositor, so a session that
// is not sway fails here rather than on the first poll.
//
// SWAYSOCK can be stale before the daemon has polled even once. A
// systemd user unit inherits the environment saved when the previous
// session started, so a compositor restart that takes the daemon down
// with it hands the replacement a path to something already gone. The
// runtime rediscovery in request cannot help there: it only runs on a
// detector that constructed at least once. So the same scan runs here.
func NewSwayWindowDetector(logger *zerolog.Logger) (*SwayWindowDetector, error) {
	path := swaySocketPath()
	if path == "" {
		return nil, ErrNoSwaySocket
	}

	// Each dial and probe bounds itself, so the constructor does not
	// impose a deadline across a scan of the whole runtime directory.
	ctx := context.Background()

	conn, err := swayDial(ctx, path)
	if err == nil {
		return &SwayWindowDetector{logger: logger, socketPath: path, conn: conn}, nil
	}

	live, discoverErr := discoverSwaySocket(ctx)
	if discoverErr != nil {
		// Report why SWAYSOCK failed, not why the scan did: the
		// environment is what the user set, and the scan is a fallback
		// that found nothing to offer.
		return nil, err
	}

	conn, dialErr := swayDial(ctx, live)
	if dialErr != nil {
		return nil, err
	}

	logger.Info().
		Str("stale", path).
		Str("adopted", live).
		Msg("SWAYSOCK named a socket that is gone, adopting the live one")
	return &SwayWindowDetector{logger: logger, socketPath: live, conn: conn}, nil
}

// ActiveWindow returns the focused window's app name and title.
func (s *SwayWindowDetector) ActiveWindow(ctx context.Context) (WindowInfo, error) {
	payload, err := s.request(ctx, swayMsgGetTree)
	if err != nil {
		return WindowInfo{}, err
	}

	var root swayNode
	if err := json.Unmarshal(payload, &root); err != nil {
		return WindowInfo{}, fmt.Errorf("decoding sway tree: %w", err)
	}

	node := focusedWindow(&root)
	if node == nil {
		return WindowInfo{}, ErrNoActiveWindow
	}
	return WindowInfo{AppName: swayAppName(node), Title: strings.TrimSpace(node.Name)}, nil
}

// Close releases the IPC connection.
func (s *SwayWindowDetector) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropConnLocked()
}

// request sends a message and returns the reply payload, recovering
// from the two ways the connection can go bad.
//
// A redial handles both a socket that was merely dropped and a request
// that failed halfway, which leaves the stream desynced with no way to
// resynchronise. When the redial fails too, the compositor may have
// restarted somewhere new, and only rediscovery gets the daemon back.
func (s *SwayWindowDetector) request(ctx context.Context, msgType uint32) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	payload, err := s.attemptLocked(ctx, msgType)
	if err == nil {
		return payload, nil
	}
	if ctx.Err() != nil {
		return nil, err
	}

	payload, err = s.attemptLocked(ctx, msgType)
	if err == nil {
		return payload, nil
	}
	if ctx.Err() != nil || !s.rediscoverLocked(ctx) {
		return nil, err
	}

	return s.attemptLocked(ctx, msgType)
}

// attemptLocked runs one round trip, dropping the connection on any
// failure so the next attempt starts from a fresh one.
func (s *SwayWindowDetector) attemptLocked(ctx context.Context, msgType uint32) ([]byte, error) {
	conn, err := s.connLocked(ctx)
	if err != nil {
		return nil, err
	}
	payload, err := swayRoundTrip(ctx, conn, msgType)
	if err != nil {
		s.dropConnLocked()
		return nil, err
	}
	return payload, nil
}

// rediscoverLocked looks for a live sway socket and adopts it,
// reporting whether the caller has something new to try.
func (s *SwayWindowDetector) rediscoverLocked(ctx context.Context) bool {
	path, err := discoverSwaySocket(ctx)
	if err != nil {
		s.logger.Debug().Err(err).Msg("sway IPC socket rediscovery found nothing")
		return false
	}
	if path == s.socketPath {
		return false
	}

	s.logger.Info().
		Str("previous", s.socketPath).
		Str("adopted", path).
		Msg("sway IPC socket moved, adopting the live one")
	s.socketPath = path
	s.dropConnLocked()
	return true
}

func (s *SwayWindowDetector) connLocked(ctx context.Context) (net.Conn, error) {
	if s.conn != nil {
		return s.conn, nil
	}
	conn, err := swayDial(ctx, s.socketPath)
	if err != nil {
		return nil, err
	}
	s.conn = conn
	return conn, nil
}

func (s *SwayWindowDetector) dropConnLocked() {
	if s.conn == nil {
		return
	}
	_ = s.conn.Close()
	s.conn = nil
}

func swayDial(ctx context.Context, path string) (net.Conn, error) {
	conn, err := dialSocket(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("connecting to sway IPC at %s: %w", path, err)
	}
	return conn, nil
}

// swayVersionProbe reports whether the peer answers GET_VERSION the way
// sway does. Discovery uses it to tell the compositor from anything
// else that happens to be listening at a matching name.
func swayVersionProbe(ctx context.Context, conn net.Conn) bool {
	payload, err := swayRoundTrip(ctx, conn, swayMsgGetVersion)
	if err != nil {
		return false
	}

	// A pointer field separates "absent" from "zero", so a reply of {}
	// does not pass for a version.
	var version struct {
		Major *int `json:"major"`
	}
	if err := json.Unmarshal(payload, &version); err != nil {
		return false
	}
	return version.Major != nil
}

// swayRoundTrip writes one request and reads its reply. The deadline
// covers both halves, so a compositor that accepts the write and then
// stops answering cannot wedge the poll loop.
func swayRoundTrip(ctx context.Context, conn net.Conn, msgType uint32) ([]byte, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(swayIOTimeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("setting sway IPC deadline: %w", err)
	}

	if err := swayWriteMessage(conn, msgType); err != nil {
		return nil, err
	}

	replyType, payload, err := swayReadMessage(conn)
	if err != nil {
		return nil, err
	}
	if replyType != msgType {
		return nil, fmt.Errorf("sway IPC reply type %d, want %d", replyType, msgType)
	}
	return payload, nil
}

// swayWriteMessage writes a payload-less request in one call, so a
// short write cannot leave a header on the wire with no body behind it.
//
// io.Writer promises a non-nil error whenever it writes less than it
// was given, so the length check below should never fire. It is here
// because the sentence above is the whole point of building the frame
// in one buffer, and a half-written header desyncs every read after
// it -- worth one comparison to not take that on trust.
func swayWriteMessage(w io.Writer, msgType uint32) error {
	buf := make([]byte, swayHeaderLen)
	copy(buf, swayMagic)
	binary.NativeEndian.PutUint32(buf[len(swayMagic):], 0)
	binary.NativeEndian.PutUint32(buf[len(swayMagic)+4:], msgType)

	written, err := w.Write(buf)
	if err != nil {
		return fmt.Errorf("writing sway IPC message: %w", err)
	}
	if written != len(buf) {
		return fmt.Errorf("writing sway IPC message: %w", io.ErrShortWrite)
	}
	return nil
}

func swayReadMessage(r io.Reader) (uint32, []byte, error) {
	header := make([]byte, swayHeaderLen)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, nil, fmt.Errorf("reading sway IPC header: %w", err)
	}
	if string(header[:len(swayMagic)]) != swayMagic {
		return 0, nil, errSwayBadMagic
	}

	length := binary.NativeEndian.Uint32(header[len(swayMagic):])
	msgType := binary.NativeEndian.Uint32(header[len(swayMagic)+4:])
	if length > swayMaxPayload {
		return 0, nil, fmt.Errorf("sway IPC payload of %d bytes is implausible", length)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("reading sway IPC payload: %w", err)
	}
	return msgType, payload, nil
}

// focusedWindow finds the one node sway marks as focused, provided it
// is a window. Focus rests on the tree's leaves, but not always on a
// window: an empty workspace holds focus itself, and "focus parent"
// leaves it on a split container. Neither is something the user is
// working in, so both read as no active window.
func focusedWindow(node *swayNode) *swayNode {
	if node.Focused && node.isWindow() {
		return node
	}
	for i := range node.Nodes {
		if found := focusedWindow(&node.Nodes[i]); found != nil {
			return found
		}
	}
	for i := range node.FloatingNodes {
		if found := focusedWindow(&node.FloatingNodes[i]); found != nil {
			return found
		}
	}
	return nil
}

func (n *swayNode) isWindow() bool {
	if n.Type != swayNodeCon && n.Type != swayNodeFloatingCon {
		return false
	}
	return len(n.Nodes) == 0 && len(n.FloatingNodes) == 0
}

// swayAppName prefers app_id, which native Wayland clients set. An
// XWayland client leaves it empty and carries WM_CLASS instead, which
// is the same string the X11 detector reports for that application.
func swayAppName(node *swayNode) string {
	if name := strings.TrimSpace(node.AppID); name != "" {
		return name
	}
	if name := strings.TrimSpace(node.WindowProperties.Class); name != "" {
		return name
	}
	return unknownApp
}
