//go:build linux

package tracker

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

const (
	testSwaySock = "/run/sway.sock"
	testDisplay  = "wayland-1"
)

// clearSessionEnv neutralises every variable the classifiers read, so
// a test asserts on what it sets rather than on the shell it ran from.
func clearSessionEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		envSwaySock, "I3SOCK", envWaylandDisplay, envSessionType, envRuntimeDir,
	} {
		t.Setenv(key, "")
	}
}

// testListen opens a listener the way the linter wants it opened.
func testListen(t *testing.T, network, address string) net.Listener {
	t.Helper()

	var config net.ListenConfig
	listener, err := config.Listen(context.Background(), network, address)
	if err != nil {
		t.Fatalf("listen %s %s: %v", network, address, err)
	}
	return listener
}

func testDial(t *testing.T, network, address string) net.Conn {
	t.Helper()

	var dialer net.Dialer
	conn, err := dialer.DialContext(context.Background(), network, address)
	if err != nil {
		t.Fatalf("dial %s %s: %v", network, address, err)
	}
	return conn
}

func TestSessionClassification(t *testing.T) {
	clearSessionEnv(t)

	tests := []struct {
		name        string
		env         map[string]string
		wantSway    string
		wantWayland bool
	}{
		{
			name:        "sway",
			env:         map[string]string{envSwaySock: testSwaySock, envWaylandDisplay: testDisplay},
			wantSway:    testSwaySock,
			wantWayland: true,
		},
		{
			// The partial-environment row: a daemon that inherited
			// SWAYSOCK alone must still read as Wayland, or it would
			// take sway IPC for windows and xprintidle for idle.
			name:        "sway with a partial environment",
			env:         map[string]string{envSwaySock: testSwaySock},
			wantSway:    testSwaySock,
			wantWayland: true,
		},
		{
			name:        "another wayland compositor",
			env:         map[string]string{envWaylandDisplay: "wayland-0"},
			wantSway:    "",
			wantWayland: true,
		},
		{
			name:        "session type alone",
			env:         map[string]string{envSessionType: "wayland"},
			wantSway:    "",
			wantWayland: true,
		},
		{
			// i3 sets I3SOCK on an X11 session, where xdotool and
			// xprintidle are both correct.
			name:        "i3 on x11",
			env:         map[string]string{"I3SOCK": "/run/i3.sock", envSessionType: "x11"},
			wantSway:    "",
			wantWayland: false,
		},
		{
			name:        "plain x11",
			env:         map[string]string{"DISPLAY": ":0"},
			wantSway:    "",
			wantWayland: false,
		},
		{
			name:        "whitespace is not a socket",
			env:         map[string]string{envSwaySock: "   "},
			wantSway:    "",
			wantWayland: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearSessionEnv(t)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}

			if got := swaySocketPath(); got != tt.wantSway {
				t.Errorf("swaySocketPath() = %q, want %q", got, tt.wantSway)
			}
			if got := waylandSession(); got != tt.wantWayland {
				t.Errorf("waylandSession() = %v, want %v", got, tt.wantWayland)
			}
		})
	}
}

func TestPeerUIDMatchesThisUser(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "s")
	listener := testListen(t, "unix", path)
	defer func() { _ = listener.Close() }()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	conn := testDial(t, "unix", path)
	defer func() { _ = conn.Close() }()

	uid, err := peerUID(conn)
	if err != nil {
		t.Fatalf("peerUID: %v", err)
	}
	if uid != currentUID() {
		t.Errorf("peerUID() = %d, want %d", uid, currentUID())
	}
}

func TestPeerUIDRejectsNonUnixConn(t *testing.T) {
	t.Parallel()

	listener := testListen(t, "tcp", "127.0.0.1:0")
	defer func() { _ = listener.Close() }()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	conn := testDial(t, "tcp", listener.Addr().String())
	defer func() { _ = conn.Close() }()

	if _, err := peerUID(conn); !errors.Is(err, errNotUnixConn) {
		t.Errorf("peerUID() error = %v, want errNotUnixConn", err)
	}
}

func TestLiveSocketsNeedsRuntimeDir(t *testing.T) {
	clearSessionEnv(t)

	// XDG_RUNTIME_DIR unset turns discovery off entirely rather than
	// falling back to a world-writable /tmp, where any local user
	// could plant a socket matching the glob.
	if _, err := liveSockets(context.Background(), "wayland-*", nil); !errors.Is(err, errNoRuntimeDir) {
		t.Errorf("liveSockets() error = %v, want errNoRuntimeDir", err)
	}
	if _, err := discoverSwaySocket(context.Background()); !errors.Is(err, errNoRuntimeDir) {
		t.Errorf("discoverSwaySocket(context.Background()) error = %v, want errNoRuntimeDir", err)
	}
	if _, err := discoverWaylandDisplay(context.Background()); !errors.Is(err, errNoRuntimeDir) {
		t.Errorf("discoverWaylandDisplay(context.Background()) error = %v, want errNoRuntimeDir", err)
	}
}

// listenAt starts a listener that accepts and immediately closes, which
// is all liveSockets needs to see when it has no probe.
func listenAt(t *testing.T, path string) {
	t.Helper()

	listener := testListen(t, "unix", path)
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
}

func TestDiscoverWaylandDisplay(t *testing.T) {
	clearSessionEnv(t)

	t.Run("one live display", func(t *testing.T) {
		clearSessionEnv(t)
		dir := t.TempDir()
		t.Setenv(envRuntimeDir, dir)
		listenAt(t, filepath.Join(dir, testDisplay))

		got, err := discoverWaylandDisplay(context.Background())
		if err != nil {
			t.Fatalf("discoverWaylandDisplay: %v", err)
		}
		if got != testDisplay {
			t.Errorf("display = %q, want wayland-1", got)
		}
	})

	t.Run("lock file is not a candidate", func(t *testing.T) {
		clearSessionEnv(t)
		dir := t.TempDir()
		t.Setenv(envRuntimeDir, dir)
		listenAt(t, filepath.Join(dir, testDisplay))
		writeFile(t, filepath.Join(dir, testDisplay+".lock"))

		got, err := discoverWaylandDisplay(context.Background())
		if err != nil {
			t.Fatalf("discoverWaylandDisplay: %v", err)
		}
		if got != testDisplay {
			t.Errorf("display = %q, want wayland-1", got)
		}
	})

	t.Run("a crashed compositor leaves its socket behind", func(t *testing.T) {
		clearSessionEnv(t)
		dir := t.TempDir()
		t.Setenv(envRuntimeDir, dir)

		// The stale inode is owned by this user and matches the glob,
		// so only the dial tells it from the live one. Without that
		// probe this counts two candidates and resolves to nothing.
		writeFile(t, filepath.Join(dir, testDisplay))
		listenAt(t, filepath.Join(dir, "wayland-2"))

		got, err := discoverWaylandDisplay(context.Background())
		if err != nil {
			t.Fatalf("discoverWaylandDisplay: %v", err)
		}
		if got != "wayland-2" {
			t.Errorf("display = %q, want wayland-2", got)
		}
	})

	t.Run("two live displays are refused", func(t *testing.T) {
		clearSessionEnv(t)
		dir := t.TempDir()
		t.Setenv(envRuntimeDir, dir)
		listenAt(t, filepath.Join(dir, testDisplay))
		listenAt(t, filepath.Join(dir, "wayland-2"))

		if _, err := discoverWaylandDisplay(context.Background()); !errors.Is(err, errAmbiguous) {
			t.Errorf("error = %v, want errAmbiguous", err)
		}
	})

	t.Run("no display at all", func(t *testing.T) {
		clearSessionEnv(t)
		t.Setenv(envRuntimeDir, t.TempDir())

		if _, err := discoverWaylandDisplay(context.Background()); !errors.Is(err, errNoCandidate) {
			t.Errorf("error = %v, want errNoCandidate", err)
		}
	})
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
