//go:build linux

package tracker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Which detectors the daemon can use comes down to two questions, and
// they are not the same question:
//
//	                    SWAYSOCK  WAYLAND_DISPLAY  window     idle
//	sway                set       set              sway IPC   swayidle
//	sway, partial env   set       unset            sway IPC   swayidle
//	other Wayland       unset     set              error      not built
//	i3 on X11           unset     unset            xdotool    xprintidle
//	plain X11           unset     unset            xdotool    xprintidle
//
// The window detector needs sway specifically, because the layout tree
// it reads is a sway feature. The idle detector needs only Wayland,
// because swayidle drives ext-idle-notify-v1, which any compositor may
// implement. Neither may fall through to its X11 counterpart on a
// Wayland session: both answer confidently and wrongly there.
//
// I3SOCK answers neither. Sway sets it alongside SWAYSOCK, so it adds
// nothing there, and under i3 it marks an X11 session where xdotool and
// xprintidle are already right -- reading it as "Wayland" would take a
// working xprintidle away from i3 users and hand them a swayidle with
// no compositor to talk to.

// socketProbeTimeout bounds one candidate's dial and probe. Discovery
// walks every match in the runtime directory, so a socket that accepts
// and then says nothing must not hold up the poll behind it.
const socketProbeTimeout = 2 * time.Second

// The variables that classify a session, named once so the production
// code and the tests that neutralise them cannot drift.
const (
	envSwaySock       = "SWAYSOCK"
	envWaylandDisplay = "WAYLAND_DISPLAY"
	envSessionType    = "XDG_SESSION_TYPE"
	envRuntimeDir     = "XDG_RUNTIME_DIR"
)

var (
	// errNoRuntimeDir reports that XDG_RUNTIME_DIR is unset, which
	// turns discovery off entirely. See discoverSwaySocket.
	errNoRuntimeDir = errors.New("XDG_RUNTIME_DIR is not set")

	// errNoCandidate reports that scanning found nothing listening.
	errNoCandidate = errors.New("no live socket found")

	// errAmbiguous reports several live sockets, which is a nested
	// compositor or a second seat. Choosing between them would produce
	// a complete and plausible timeline of the wrong session, so
	// discovery stops instead.
	errAmbiguous = errors.New("several live sockets, refusing to guess")

	errNotUnixConn = errors.New("connection is not a unix socket")
)

// swaySocketPath returns sway's IPC socket, or "" when the environment
// advertises none.
func swaySocketPath() string {
	return strings.TrimSpace(os.Getenv(envSwaySock))
}

// currentUID reports this process's UID in the form peer credentials
// arrive in.
func currentUID() uint32 {
	return uint32(os.Getuid()) //nolint:gosec // A UID is never negative.
}

// waylandSession reports whether the daemon is running under a Wayland
// compositor. Any one of the three variables is enough, because a
// daemon can inherit an environment that was imported piecemeal and
// guessing X11 costs more than guessing Wayland: the X11 idle detector
// answers a Wayland session with a confident wrong number, while the
// Wayland one degrades to reporting no idle at all.
//
// SWAYSOCK counts even though it is the narrower fact. Left out, a
// daemon that inherited only SWAYSOCK would read windows over sway IPC
// and idle time from xprintidle -- the two halves disagreeing about
// the session, with the idle half wrong.
func waylandSession() bool {
	if swaySocketPath() != "" {
		return true
	}
	if strings.TrimSpace(os.Getenv(envWaylandDisplay)) != "" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv(envSessionType)), "wayland")
}

// runtimeDir returns XDG_RUNTIME_DIR, or "" when it is unset.
//
// Sway falls back to /tmp here and the daemon deliberately does not.
// Sway writing to a world-writable directory is not the same as this
// daemon reading from one: any local user can create
// sway-ipc.<our-uid>.<anything>.sock in /tmp, answer GET_TREE with
// whatever they like, and have their fabrications uploaded as the
// user's activity. XDG_RUNTIME_DIR is mode 0700 and owned by the user,
// so nobody else can put a socket in it.
func runtimeDir() string {
	return strings.TrimSpace(os.Getenv(envRuntimeDir))
}

// socketProbe reports whether a candidate connection belongs to the
// service being looked for. A nil probe accepts any peer that passed
// the owner check.
type socketProbe func(context.Context, net.Conn) bool

// dialSocket opens a unix socket under the probe timeout.
func dialSocket(ctx context.Context, path string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: socketProbeTimeout}
	return dialer.DialContext(ctx, "unix", path) //nolint:wrapcheck // Callers add the context they have.
}

// liveSockets returns the paths under XDG_RUNTIME_DIR matching pattern
// that accept a connection, run as this user, and satisfy probe.
//
// Connectability is not a formality. A compositor that crashed rather
// than exited cleanly leaves its socket inode behind, owned by the user
// and matching the glob, so a name filter and an owner check both pass
// on a dead session. Only the dial tells the two apart.
func liveSockets(ctx context.Context, pattern string, probe socketProbe) ([]string, error) {
	dir := runtimeDir()
	if dir == "" {
		return nil, errNoRuntimeDir
	}

	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return nil, fmt.Errorf("scanning %s for %s: %w", dir, pattern, err)
	}

	live := make([]string, 0, len(matches))
	for _, path := range matches {
		if acceptCandidate(ctx, path, probe) {
			live = append(live, path)
		}
	}
	// Deterministic order keeps the ambiguity message and the tests
	// stable; which one is chosen never depends on it, because more
	// than one candidate is refused outright.
	sort.Strings(live)
	return live, nil
}

func acceptCandidate(ctx context.Context, path string, probe socketProbe) bool {
	// Each candidate gets its own deadline, so one socket that accepts
	// and then says nothing cannot consume the whole scan's budget.
	ctx, cancel := context.WithTimeout(ctx, socketProbeTimeout)
	defer cancel()

	conn, err := dialSocket(ctx, path)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()

	uid, err := peerUID(conn)
	if err != nil || uid != currentUID() {
		return false
	}
	return probe == nil || probe(ctx, conn)
}

// peerUID returns the UID the process on the other end of conn runs as.
//
// Checking the path's owner with stat would leave a window between the
// check and the connect. SO_PEERCRED describes the peer of the
// connection already in hand, so there is nothing to race.
func peerUID(conn net.Conn) (uint32, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errNotUnixConn
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("taking the raw connection: %w", err)
	}

	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, fmt.Errorf("reading peer credentials: %w", err)
	}
	if credErr != nil {
		return 0, fmt.Errorf("reading peer credentials: %w", credErr)
	}
	return cred.Uid, nil
}

// discoverSwaySocket finds sway's socket by scanning for it, for the
// case where the path in SWAYSOCK has died.
//
// Sway names its socket after its own PID, so a restarted compositor
// usually listens somewhere new while SWAYSOCK still holds the old
// path. Re-reading the variable does not help -- a process cannot see
// later edits to its own environment -- so the only way back without a
// daemon restart is to look.
func discoverSwaySocket(ctx context.Context) (string, error) {
	pattern := fmt.Sprintf("sway-ipc.%d.*.sock", currentUID())
	live, err := liveSockets(ctx, pattern, swayVersionProbe)
	if err != nil {
		return "", err
	}
	return exactlyOne(live)
}

// discoverWaylandDisplay finds the compositor's display socket, so a
// restarted swayidle dials the session that exists rather than the one
// named in an environment inherited at startup.
//
// The .lock file sitting beside each display socket matches the glob
// and is filtered out by the dial: a regular file is not something a
// unix socket connects to.
func discoverWaylandDisplay(ctx context.Context) (string, error) {
	live, err := liveSockets(ctx, "wayland-*", nil)
	if err != nil {
		return "", err
	}
	path, err := exactlyOne(live)
	if err != nil {
		return "", err
	}
	return filepath.Base(path), nil
}

// exactlyOne enforces the rule discovery turns on: adopt a candidate
// only when it is the only one. Two live sockets mean a nested
// compositor, or two seats, or two ttys, and picking the wrong one
// produces a complete, plausible, entirely wrong timeline.
func exactlyOne(live []string) (string, error) {
	switch len(live) {
	case 0:
		return "", errNoCandidate
	case 1:
		return live[0], nil
	default:
		return "", fmt.Errorf("%w: %s", errAmbiguous, strings.Join(live, ", "))
	}
}
