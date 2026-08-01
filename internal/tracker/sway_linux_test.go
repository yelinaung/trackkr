//go:build linux

package tracker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

const (
	swayNodeTypeRoot = "root"
	appFoot          = "foot"
	titleVim         = "vim"
	appFirefox       = "firefox"
)

const swayVersionReply = `{"major":1,"minor":9,"patch":0,"human_readable":"1.9"}`

// writeSwayReply frames a reply the way sway does. The production
// writer only ever sends empty payloads, so tests need their own.
func writeSwayReply(w io.Writer, msgType uint32, payload []byte) error {
	length := uint32(len(payload)) //nolint:gosec // Test payloads are a few hundred bytes.

	buf := make([]byte, swayHeaderLen+len(payload))
	copy(buf, swayMagic)
	binary.NativeEndian.PutUint32(buf[len(swayMagic):], length)
	binary.NativeEndian.PutUint32(buf[len(swayMagic)+4:], msgType)
	copy(buf[swayHeaderLen:], payload)

	_, err := w.Write(buf)
	return err
}

// swayHandler answers GET_TREE with tree and GET_VERSION the way a
// real compositor would.
func swayHandler(tree string) func(net.Conn) {
	return func(conn net.Conn) {
		for {
			msgType, _, err := swayReadMessage(conn)
			if err != nil {
				return
			}

			var payload string
			switch msgType {
			case swayMsgGetTree:
				payload = tree
			case swayMsgGetVersion:
				payload = swayVersionReply
			default:
				return
			}
			if err := writeSwayReply(conn, msgType, []byte(payload)); err != nil {
				return
			}
		}
	}
}

// fakeSway is a stand-in compositor. Close takes the accepted
// connections down with the listener, because a compositor that exits
// drops the clients holding one -- closing only the listener would
// leave the detector happily talking to a session that is gone.
type fakeSway struct {
	listener net.Listener

	mu     sync.Mutex
	conns  []net.Conn
	closed bool
}

func (f *fakeSway) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return
	}
	f.closed = true
	_ = f.listener.Close()
	for _, conn := range f.conns {
		_ = conn.Close()
	}
}

func (f *fakeSway) track(conn net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return false
	}
	f.conns = append(f.conns, conn)
	return true
}

// serveSway starts a fake compositor at path.
func serveSway(t *testing.T, path string, handle func(net.Conn)) *fakeSway {
	t.Helper()

	listener := testListen(t, "unix", path)
	server := &fakeSway{listener: listener}
	t.Cleanup(server.Close)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if !server.track(conn) {
				_ = conn.Close()
				return
			}
			go handle(conn)
		}
	}()
	return server
}

// swaySocketName builds a name matching the glob discovery scans for.
func swaySocketName(dir string, pid int) string {
	return filepath.Join(dir, fmt.Sprintf("sway-ipc.%d.%d.sock", currentUID(), pid))
}

func testLogger() *zerolog.Logger {
	logger := zerolog.Nop()
	return &logger
}

const treeOneWindow = `{
  "type": "root",
  "nodes": [{
    "type": "output",
    "nodes": [{
      "type": "workspace",
      "nodes": [
        {"type": "con", "name": "vim", "app_id": "foot", "focused": true}
      ]
    }]
  }]
}`

func newTestDetector(t *testing.T, tree string) *SwayWindowDetector {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "s")
	serveSway(t, path, swayHandler(tree))
	t.Setenv(envSwaySock, path)

	detector, err := NewSwayWindowDetector(testLogger())
	if err != nil {
		t.Fatalf("NewSwayWindowDetector: %v", err)
	}
	t.Cleanup(detector.Close)
	return detector
}

func TestSwayActiveWindow(t *testing.T) {
	clearSessionEnv(t)
	detector := newTestDetector(t, treeOneWindow)

	info, err := detector.ActiveWindow(context.Background())
	if err != nil {
		t.Fatalf("ActiveWindow: %v", err)
	}
	if info.AppName != appFoot {
		t.Errorf("AppName = %q, want foot", info.AppName)
	}
	if info.Title != titleVim {
		t.Errorf("Title = %q, want vim", info.Title)
	}
}

func TestSwayNoSocketEnv(t *testing.T) {
	clearSessionEnv(t)

	if _, err := NewSwayWindowDetector(testLogger()); !errors.Is(err, ErrNoSwaySocket) {
		t.Errorf("error = %v, want ErrNoSwaySocket", err)
	}
}

func TestSwayReconnectsOnSamePath(t *testing.T) {
	clearSessionEnv(t)
	detector := newTestDetector(t, treeOneWindow)

	// Prime the held connection, then have the server drop it.
	if _, err := detector.ActiveWindow(context.Background()); err != nil {
		t.Fatalf("first ActiveWindow: %v", err)
	}
	detector.mu.Lock()
	detector.dropConnLocked()
	detector.mu.Unlock()

	if _, err := detector.ActiveWindow(context.Background()); err != nil {
		t.Fatalf("ActiveWindow after a dropped connection: %v", err)
	}
}

func TestSwayRediscoversMovedSocket(t *testing.T) {
	clearSessionEnv(t)

	// A restarted sway listens at a path built from its new PID, while
	// SWAYSOCK still names the dead one. Re-reading the variable would
	// find the same stale path, so only a scan gets the daemon back.
	dir := t.TempDir()
	t.Setenv(envRuntimeDir, dir)

	oldPath := swaySocketName(dir, 111)
	oldServer := serveSway(t, oldPath, swayHandler(treeOneWindow))
	t.Setenv(envSwaySock, oldPath)

	detector, err := NewSwayWindowDetector(testLogger())
	if err != nil {
		t.Fatalf("NewSwayWindowDetector: %v", err)
	}
	t.Cleanup(detector.Close)

	if _, err := detector.ActiveWindow(context.Background()); err != nil {
		t.Fatalf("first ActiveWindow: %v", err)
	}

	// The compositor goes down: the socket is unlinked and the held
	// connection dies with it.
	oldServer.Close()
	serveSway(t, swaySocketName(dir, 222), swayHandler(treeOneWindow))

	info, err := detector.ActiveWindow(context.Background())
	if err != nil {
		t.Fatalf("ActiveWindow after the compositor moved: %v", err)
	}
	if info.AppName != appFoot {
		t.Errorf("AppName = %q, want foot", info.AppName)
	}
	if detector.socketPath == oldPath {
		t.Error("detector kept the dead socket path")
	}
}

func TestSwayRefusesAmbiguousSockets(t *testing.T) {
	clearSessionEnv(t)

	dir := t.TempDir()
	t.Setenv(envRuntimeDir, dir)
	serveSway(t, swaySocketName(dir, 111), swayHandler(treeOneWindow))
	serveSway(t, swaySocketName(dir, 222), swayHandler(treeOneWindow))

	// Two live sockets mean a nested sway or a second seat. Guessing
	// would produce a complete, plausible timeline of the wrong
	// session.
	if _, err := discoverSwaySocket(context.Background()); !errors.Is(err, errAmbiguous) {
		t.Errorf("error = %v, want errAmbiguous", err)
	}
}

func TestSwayIgnoresStaleSocketFile(t *testing.T) {
	clearSessionEnv(t)

	dir := t.TempDir()
	t.Setenv(envRuntimeDir, dir)
	writeFile(t, swaySocketName(dir, 111))
	live := swaySocketName(dir, 222)
	serveSway(t, live, swayHandler(treeOneWindow))

	got, err := discoverSwaySocket(context.Background())
	if err != nil {
		t.Fatalf("discoverSwaySocket: %v", err)
	}
	if got != live {
		t.Errorf("discovered %q, want %q", got, live)
	}
}

func TestSwayDiscoveryRejectsImpostors(t *testing.T) {
	clearSessionEnv(t)

	tests := []struct {
		name   string
		handle func(net.Conn)
	}{
		{
			name: "answers with garbage",
			handle: func(conn net.Conn) {
				_, _, _ = swayReadMessage(conn)
				_ = writeSwayReply(conn, swayMsgGetVersion, []byte("not json"))
			},
		},
		{
			name: "a reply with no version in it",
			handle: func(conn net.Conn) {
				_, _, _ = swayReadMessage(conn)
				_ = writeSwayReply(conn, swayMsgGetVersion, []byte(`{}`))
			},
		},
		{
			name: "closes on the probe",
			handle: func(conn net.Conn) {
				_ = conn.Close()
			},
		},
		{
			// Accepting and then saying nothing must time out rather
			// than wedge the scan behind it.
			name: "accepts and never replies",
			handle: func(conn net.Conn) {
				time.Sleep(2 * swayIOTimeout)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearSessionEnv(t)
			dir := t.TempDir()
			t.Setenv(envRuntimeDir, dir)
			serveSway(t, swaySocketName(dir, 111), tt.handle)

			if _, err := discoverSwaySocket(context.Background()); !errors.Is(err, errNoCandidate) {
				t.Errorf("error = %v, want errNoCandidate", err)
			}
		})
	}
}

func TestSwayReadMessageRejectsBadFrames(t *testing.T) {
	t.Parallel()

	t.Run("bad magic", func(t *testing.T) {
		t.Parallel()
		frame := make([]byte, swayHeaderLen)
		copy(frame, "not-it")

		if _, _, err := swayReadMessage(bytes.NewReader(frame)); !errors.Is(err, errSwayBadMagic) {
			t.Errorf("error = %v, want errSwayBadMagic", err)
		}
	})

	t.Run("implausible payload length", func(t *testing.T) {
		t.Parallel()
		// Allocating what this claims would take the daemon down; the
		// bound has to reject it before make() sees it.
		frame := make([]byte, swayHeaderLen)
		copy(frame, swayMagic)
		binary.NativeEndian.PutUint32(frame[len(swayMagic):], swayMaxPayload+1)

		_, _, err := swayReadMessage(bytes.NewReader(frame))
		if err == nil || !strings.Contains(err.Error(), "implausible") {
			t.Errorf("error = %v, want an implausible-length error", err)
		}
	})

	t.Run("truncated payload", func(t *testing.T) {
		t.Parallel()
		frame := make([]byte, swayHeaderLen)
		copy(frame, swayMagic)
		binary.NativeEndian.PutUint32(frame[len(swayMagic):], 128)

		if _, _, err := swayReadMessage(bytes.NewReader(frame)); err == nil {
			t.Error("want an error for a payload that never arrives")
		}
	})
}

func TestFocusedWindow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		tree  swayNode
		want  string
		found bool
	}{
		{
			name: "a focused leaf",
			tree: swayNode{Type: swayNodeTypeRoot, Nodes: []swayNode{
				{Type: swayNodeCon, Name: titleVim, Focused: true},
			}},
			want:  titleVim,
			found: true,
		},
		{
			name: "a floating window",
			tree: swayNode{Type: swayNodeTypeRoot, FloatingNodes: []swayNode{
				{Type: swayNodeFloatingCon, Name: "pavucontrol", Focused: true},
			}},
			want:  "pavucontrol",
			found: true,
		},
		{
			name: "nested under an output and a workspace",
			tree: swayNode{Type: swayNodeTypeRoot, Nodes: []swayNode{
				{Type: "output", Nodes: []swayNode{
					{Type: "workspace", Nodes: []swayNode{
						{Type: swayNodeCon, Name: appFirefox, Focused: true},
					}},
				}},
			}},
			want:  appFirefox,
			found: true,
		},
		{
			// An empty workspace holds focus itself, which is the same
			// state as a bare desktop on X11.
			name: "an empty focused workspace",
			tree: swayNode{Type: swayNodeTypeRoot, Nodes: []swayNode{
				{Type: "workspace", Name: "3", Focused: true},
			}},
			found: false,
		},
		{
			// "focus parent" leaves focus on a split container, which
			// is not something the user is working in.
			name: "a focused split container",
			tree: swayNode{Type: swayNodeTypeRoot, Nodes: []swayNode{
				{Type: swayNodeCon, Focused: true, Nodes: []swayNode{
					{Type: swayNodeCon, Name: titleVim},
				}},
			}},
			found: false,
		},
		{
			name: "nothing focused",
			tree: swayNode{Type: swayNodeTypeRoot, Nodes: []swayNode{
				{Type: swayNodeCon, Name: titleVim},
			}},
			found: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := focusedWindow(&tt.tree)
			if (got != nil) != tt.found {
				t.Fatalf("focusedWindow() found = %v, want %v", got != nil, tt.found)
			}
			if got != nil && got.Name != tt.want {
				t.Errorf("focusedWindow().Name = %q, want %q", got.Name, tt.want)
			}
		})
	}
}

func TestSwayAppName(t *testing.T) {
	t.Parallel()

	xwayland := swayNode{}
	xwayland.WindowProperties.Class = "Gimp-2.10"

	blankClass := swayNode{AppID: "  "}
	blankClass.WindowProperties.Class = "  "

	tests := []struct {
		name string
		node swayNode
		want string
	}{
		{"native wayland", swayNode{AppID: appFoot}, appFoot},
		{"xwayland falls back to class", xwayland, "Gimp-2.10"},
		{"neither", swayNode{}, unknownApp},
		{"whitespace counts as neither", blankClass, unknownApp},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := swayAppName(&tt.node); got != tt.want {
				t.Errorf("swayAppName() = %q, want %q", got, tt.want)
			}
		})
	}
}
