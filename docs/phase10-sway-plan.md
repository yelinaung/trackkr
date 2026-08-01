# Phase 10: Sway / Wayland Support

## Context

The daemon's Linux support is X11 support. `NewWindowDetector` shells
out to `xdotool` and `xprop`; `NewIdleDetectorOrNop` shells out to
`xprintidle`. Phase 2 noted the gap and deferred Wayland.

Both fail on a sway session, and they fail differently.

`xdotool getactivewindow` talks to the X server. Under sway that server
is XWayland, which knows only about the X clients sway happens to host.
Focus a native Wayland window and `getactivewindow` returns whatever
XWayland last saw focused, or nothing. The daemon records the wrong
window, or records none.

`xprintidle` reads the X screensaver extension's idle counter, which
XWayland maintains from the events XWayland itself receives. Typing in
a native Wayland application never touches that counter. It climbs
while the user works, the tracker crosses the idle threshold, and
recording stops on a session that is very much active.

The second failure is worse, because it is silent. An erroring detector
leaves a warning in the log. A detector that confidently returns a
wrong number leaves nothing, and corrupts the timeline.

The interfaces do not change. `WindowDetector` and `IdleDetector` keep
their one-method shape, and the tracking loop never learns which
compositor it is on.

## Decisions

### Window detection: sway's IPC socket, not swaymsg

Sway speaks the i3 IPC protocol over a unix socket at `$SWAYSOCK`: the
magic string `i3-ipc`, then payload length and message type as
native-endian `uint32`s, then a JSON payload. `GET_TREE` (type 4)
returns the whole layout tree, where exactly one node carries
`"focused": true`.

Shelling out to `swaymsg -t get_tree` on every poll would match what
the X11 detector does, and would cost one process spawn every three
seconds for the life of the session.

Speaking the protocol directly costs roughly 120 lines of framing and
one held connection. It spawns nothing, works whether or not `swaymsg`
is installed, and tests more easily: a fake IPC server on a unix socket
in `t.TempDir()` drives the real code path, whereas faking `swaymsg`
means faking `exec`.

Speak it directly. The framing is small and fixed, and the tree is
ordinary JSON.

The held connection needs one reconnect on failure. A request that
fails halfway leaves the stream desynced with no way to resynchronise,
and a dropped socket is fine to redial; a fresh connection recovers
either. Without it the detector stays broken until the daemon restarts.

### A restarted compositor may move its socket

Sway builds its socket path from its own PID:

```text
$XDG_RUNTIME_DIR/sway-ipc.<uid>.<pid>.sock
```

A restarted sway usually listens somewhere new while `SWAYSOCK` in the
daemon's environment still names the dead one. Not always: sway reuses
an inherited `SWAYSOCK` when the file at that path has gone, so a
restart can land back on the same name, and an ordinary redial handles
it. Where the name moves, redialling fails for good, because a process
cannot see later edits to its own environment and re-reading the
variable returns the same stale path.

Whether the daemon survives to care depends on how it started. A sway
`exec` line makes it a child of the compositor, and it will usually go
down with the session, though an ordinary child can outlive its parent.
A systemd user unit certainly outlives it, and would otherwise fail
every poll for the rest of the day behind nothing louder than a warning
per tick.

So the reconnect gets one fallback. When the captured path will not
connect, scan for candidates:

```text
$XDG_RUNTIME_DIR/sway-ipc.<uid>.*.sock
```

#### Discovery has to authenticate what it finds

A connectable pathname proves nothing. Anyone who can create a file in
the scanned directory can put a socket there, name it to match, answer
`GET_TREE` with whatever they like, and watch the daemon upload
fabricated applications and window titles to the server as the user's
activity. A plausible fake timeline the user has no reason to distrust
does more damage than no timeline at all.

Three restrictions gate adoption, and all three must pass.

**`XDG_RUNTIME_DIR` or nothing.** Sway falls back to `/tmp` when the
variable is unset, and an earlier draft of this plan followed it there.
`/tmp` is world-writable: every other local user can create
`sway-ipc.<our-uid>.<anything>.sock` in it, which turns the glob into
an invitation. `XDG_RUNTIME_DIR` is mode 0700 and owned by the user, so
nobody else can put a socket in it. Leave the variable unset and
discovery stops entirely; the daemon keeps the `SWAYSOCK` it was given,
which came from the user's own session, and fails loudly when that path
is dead.

**The peer must run as this user.** Read the connected socket's
`SO_PEERCRED` and require the peer's UID to equal `os.Getuid()`.
Checking the path's owner with `stat` leaves a window between the check
and the connect. `SO_PEERCRED` describes the process on the other end
of the connection already in hand, so nothing can race it.
`golang.org/x/sys/unix` is already in the module graph.

**It must speak the protocol.** Send `GET_VERSION` (type 7) under the
usual deadline and require a well-formed reply of the same type that
parses as sway's version JSON. The exchange is cheap and read-only, and
it separates sway from any other service listening at a matching name.

Over what survives:

```text
exactly one authenticated candidate -> adopt it, log the new path
zero, or more than one              -> keep failing, and say why
```

Two live sway sockets mean a nested sway, or two seats, or two ttys.
Picking the wrong one produces a complete, plausible, entirely wrong
timeline, so discovery declines to choose. One live socket is the
overwhelmingly common case and carries no ambiguity.

The daemon logs the adoption, because a data source that changes
underneath it deserves a line in the log even when the choice is safe.

### App name: app_id, falling back to WM_CLASS

Native Wayland clients set `app_id`. XWayland clients leave it null and
carry `window_properties.class`, the string the X11 detector pulls out
of `WM_CLASS`. Preferring `app_id` and falling back to `class` reports
one name for an application whether the user runs it on X11 or on sway,
so a device that switches keeps one history instead of two.

### Focus is not always a window

`focused` marks the focused container, which is not always something
the user works in. An empty workspace holds focus itself. `focus
parent` leaves focus on a split container. Both read as
`ErrNoActiveWindow`, which the tracker already handles as a bare
desktop on X11.

The focused node counts as a window only when its type is `con` or
`floating_con` and it has no children.

### Idle detection: swayidle

Wayland has no equivalent of the X screensaver extension. A client
cannot ask how long the user has been idle; it can only ask to be told
when a timeout it picks has elapsed, through `ext-idle-notify-v1` (sway
1.9+). The value the poll loop wants exists nowhere on the system as
something to query.

Implementing `ext-idle-notify-v1` natively means a hand-rolled Wayland
client: connect to the display socket, walk the registry, bind
`wl_seat` and `ext_idle_notifier_v1`, decode the event stream. Several
hundred lines of wire protocol would reimplement a program that ships
with sway.

Asking logind or a portal fails on the facts.
`org.freedesktop.ScreenSaver` and `org.gnome.Mutter.IdleMonitor` both
expose an idle time over D-Bus, and neither exists on sway.

Run `swayidle` as a child process. It already owns the protocol. Give
it the tracker's own idle threshold and two commands whose output the
daemon reads:

```sh
swayidle -w -C /dev/null timeout <seconds> 'echo trackkr-idle' resume 'echo trackkr-resume'
```

Both flags carry weight.

`-C /dev/null`: swayidle treats config-file events and command-line
events as cumulative, and a sway user already runs one instance from
their config, the one that locks the screen, dims the backlight and
suspends the machine. A second instance without the flag inherits every
one of those, so the tracker would lock the session a second time and
suspend the laptop out from under the user, purely as a side effect of
tracking. Pointing the config at an empty file leaves the two
`timeout`/`resume` pairs on the command line and nothing else.

`-w`: without it swayidle double-forks each command and moves on, so
two unrelated processes write the two markers with no ordering between
them. Idle and resume arriving close together, a keypress right on the
threshold, can reach the daemon's pipe reversed, and a `trackkr-resume`
overtaken by its own `trackkr-idle` leaves the detector idle for a user
sitting there typing. `-w` makes swayidle wait for each command before
continuing.

The two flags depend on each other. `-w` blocks on whatever commands
are configured, and `-C /dev/null` guarantees the only ones configured
are two `echo`s.

The event pair converts to a duration cleanly. When `trackkr-idle`
arrives at time T, the user stopped at `T - timeout`, so the detector
records `idleSince = T - timeout` and `IdleTime` returns
`now - idleSince`. The tracker's existing backdating
(`endedAt := now.Add(-idleTime)`) then lands on the moment the user
stopped instead of the moment the notification arrived.
`trackkr-resume` clears it to zero.

`swayidle` takes its timeout in whole seconds, and `idle_threshold`
accepts any positive `time.Duration` — `Config.Validate` passes `500ms`
and `1.5s`. The detector derives an effective timeout, and everything
downstream uses that instead of the configured value:

```text
effective = ceil(idle_threshold to whole seconds)
            minimum 1s, maximum 2147483s
```

Ceiling, not rounding. The tracker tests
`idleTime >= cfg.IdleThreshold`, and `IdleTime` at the moment of the
event reports exactly the effective timeout. Round `1.5s` down to `1s`
and that comparison never holds: the daemon would run a `swayidle`
whose events the tracker ignores, and would record no idle period at
all. Rounding up reports idle slightly late, which the poll interval
already does. The detector logs the effective value at startup when it
differs from the configured one, and backdates with it.

The upper bound belongs to swayidle. It keeps the timeout in a signed C
`int` of milliseconds and computes `seconds * 1000`, so anything past
2,147,483s overflows silently into a timeout that is negative or
nonsense. Go durations reach 292 years and `Config.Validate` asks only
for positive, so a config can get there. A threshold above the bound
fails construction and the daemon takes `NopIdleDetector`, the honest
reading of that config anyway: someone asking for a 25-day idle
threshold is asking never to be marked idle.

The ceiling needs computing without the usual
`(d + time.Second - 1) / time.Second` trick, which overflows `int64`
for a duration near the maximum — the very input the bound exists to
catch, arriving before the bound can look at it. Divide and check the
remainder:

```go
secs := d / time.Second
if d%time.Second != 0 {
    secs++
}
```

Startup bakes the timeout into the child process, so changing
`idle_threshold` needs a daemon restart. It already does, since the
config is read once.

`swayidle` honours idle inhibitors, so a full-screen video keeps the
session non-idle. The user asked their compositor for that behaviour,
and inheriting it is right.

When `swayidle` dies, and a compositor restart takes it along, the
detector restarts it after a short delay and clears any idle state it
was holding, instead of reporting a stale idle that never resumes.

That restart faces the stale environment one variable over. `swayidle`
connects to `$WAYLAND_DISPLAY`, and a new compositor may pick a
different display name, in which case every replacement child dials a
socket that is gone and exits immediately: a restart loop that never
recovers and reports no idle for the rest of the session.

So the supervisor re-resolves the display before each restart.
`XDG_RUNTIME_DIR` only, sockets matching `wayland-*` and not `*.lock`,
connectable, owner checked, adopted only when exactly one candidate
survives. The resolved name goes into the child's environment. When
nothing resolves, the supervisor keeps the inherited value, which is
right whenever the display name was reused.

Connectable does real work in that list. A compositor that crashed
rather than exited cleanly leaves its socket inode behind, owned by the
user and matching the glob, so the name filter and the owner check both
pass on a dead display. The replacement picks `wayland-2`, discovery
counts two candidates, declines to guess, and the supervisor keeps the
stale inherited value forever — the failure this resolution exists to
fix, reintroduced by the filter meant to fix it. A short connect drops
the corpse and leaves one candidate standing.

No protocol probe balances the sway socket's `GET_VERSION` here,
because speaking enough Wayland to verify a compositor is the work this
phase declined at the start. The 0700 directory, the owner check and
the connect carry the weight, and a wrong display name makes `swayidle`
fail rather than lie.

### One rule for classifying the session

Both detectors need to know what they run under, and they must agree.
`SWAYSOCK` and `WAYLAND_DISPLAY` answer different questions, so each
detector asks the one matching what it needs:

```text
                    SWAYSOCK  WAYLAND_DISPLAY  window     idle
sway                set       set              sway IPC   swayidle
sway, partial env   set       unset            sway IPC   swayidle
other Wayland       unset     set              error      not built
i3 on X11           unset     unset            xdotool    xprintidle
plain X11           unset     unset            xdotool    xprintidle
```

The window detector keys off `SWAYSOCK`, because it needs sway
specifically: the IPC tree is a sway feature, not a Wayland one. Its
selection has three arms.

```text
SWAYSOCK set        -> SwayWindowDetector, or its error
waylandSession()    -> ErrUnsupportedPlatform
otherwise           -> XWindowDetector
```

Hyprland, river and GNOME are Wayland sessions with no `SWAYSOCK`.
Falling through to `xdotool` there reproduces the silent XWayland
corruption this phase exists to remove, a stale X client reported as
the focused window forever. An unsupported compositor has to say so.

The idle detector keys off Wayland, because `swayidle` works against
any compositor implementing `ext-idle-notify-v1`, whatever its name
suggests.

Wayland means any of `SWAYSOCK`, `WAYLAND_DISPLAY`, or
`XDG_SESSION_TYPE=wayland`. `SWAYSOCK` belongs in that list even as the
narrower fact: a daemon whose environment arrived piecemeal can hold
`SWAYSOCK` and nothing else, and leaving it out would put that daemon
on sway IPC for windows and `xprintidle` for idle, the two halves
disagreeing about the session with the idle half wrong in the direction
that loses data.

`I3SOCK` is out. Sway sets it alongside `SWAYSOCK`, where it adds
nothing, and i3 sets it on an X11 session where `xdotool` and
`xprintidle` already work. Keying the window detector off it would be
harmless. Keying the idle detector off the same helper, which an
earlier draft did, would put i3 users on a `swayidle` with no
compositor to talk to and take away a working `xprintidle`.

### Neither detector falls back across the X11 boundary

`xprintidle` runs fine under XWayland and returns a plausible, wrong
number. When `swayidle` is missing, the idle detector logs what to
install and returns `NopIdleDetector`, which reports zero.
Over-recording an unattended session is recoverable; silently dropping
an active one is not.

The window detector does not fall back either. `SWAYSOCK` set means the
session is sway, and `xdotool` under sway does not fail loudly: it asks
XWayland, which answers about whichever X client it last saw focused. A
wrong window recorded with full confidence is worse than no window at
all. So a sway session whose IPC will not connect returns the error, and
`cmd/trackkrd/main.go` already handles it — fatal, unless the browser
extension gives the daemon another reason to run.

`xdotool` stays the detector only for sessions that are not Wayland at
all.

## New Files

```text
internal/tracker/
  session_linux.go         # swaySocketPath, waylandSession, discovery
  session_linux_test.go    # the classification table, socket scanning
  sway_linux.go            # IPC framing, tree walk, SwayWindowDetector
  sway_linux_test.go       # fake IPC server over a unix socket
  idle_sway_linux.go       # SwayIdleDetector: swayidle supervisor
  idle_sway_linux_test.go  # markers, effective timeout, supervisor
```

## Changed Files

```text
internal/tracker/window_linux.go     # sway when SWAYSOCK, no X11 fallback
internal/tracker/idle_linux.go       # swayidle on Wayland, else xprintidle
internal/tracker/idle_linux_test.go  # detector selection per session
cmd/trackkrd/main.go                 # close the idle detector on shutdown
go.mod                               # x/sys becomes direct, for SO_PEERCRED
README.md                            # Wayland dependencies
```

## Steps

1. `session_linux.go`: the two classifiers from the table,
   `swaySocketPath()` (`SWAYSOCK` only) and `waylandSession()`. One
   file, so the rule lives in one place and both detectors read it.
   The discovery helpers answer the same question `swaySocketPath`
   does, so they live there too: `runtimeDir()` returning "" when
   `XDG_RUNTIME_DIR` is unset (no `/tmp` fallback), `peerUID(net.Conn)`
   over `SO_PEERCRED`, and a `liveSockets(glob)` helper that globs,
   connects, and owner-checks. `discoverSwaySocket()` and
   `discoverWaylandDisplay()` both build on it, differing only in the
   pattern and in the `GET_VERSION` probe the sway side adds. Factoring
   it out is the point: the idle side skipping the connect is the bug
   this step guards against.

2. `sway_linux.go`: framing (`swayWriteMessage`, `swayReadMessage`),
   `swayNode`, `focusedWindow`, `swayAppName`, the `GET_VERSION` probe
   discovery authenticates with, and `SwayWindowDetector` with its
   reconnect, its socket rediscovery and `Close`. The detector's socket
   path becomes mutable state instead of a value fixed at construction.

3. `window_linux.go`: `NewWindowDetector` gets the three arms above.
   Sway detector or its error when `swaySocketPath()` is non-empty,
   `ErrUnsupportedPlatform` on any other Wayland session, and
   `NewXWindowDetector` only when neither holds. It takes the logger it
   currently ignores.

4. `idle_sway_linux.go`: `SwayIdleDetector` — `effectiveTimeout` from
   the configured threshold (overflow-safe ceiling, refusing anything
   past swayidle's bound), the supervise loop with display
   re-resolution before each restart, marker parsing split behind an
   `io.Reader`, `IdleTime` off an injectable clock, and a `Close` that
   stops the child and joins the supervisor. Command construction goes
   behind a field so tests can substitute `sh`:

    ```go
    newCmd func(ctx context.Context, timeout time.Duration) *exec.Cmd
    ```

5. `idle_linux.go`: `NewIdleDetectorOrNop` takes the config's threshold
   and picks `SwayIdleDetector` when `waylandSession()`,
   `NopIdleDetector` when that construction fails for either reason (no
   `swayidle` on `PATH`, or a threshold past the bound), and
   `XIdleDetector` otherwise.

6. `cmd/trackkrd/main.go`: the idle detector now owns a process, so it
   needs the `Close` the window detector already gets. Factor the
   once-guarded closer both use into one helper.

7. README: `swayidle` as a dependency, and the note that the daemon
   needs `SWAYSOCK` and `WAYLAND_DISPLAY` in its environment. A systemd
   user unit gets them from `systemctl --user import-environment`, a
   sway `exec` line gets them for free.

## Tests

Everything below runs on any Linux box. No compositor, no `swayidle`,
no X server.

`sway_linux_test.go` stands up a `net.Listener` on a unix socket in
`t.TempDir()`, serves canned trees, and points the detector at it:

- `app_id` wins; XWayland's `class` is the fallback; neither gives
  `unknown`
- title comes from the node's `name`
- a focused workspace, a focused split container, and a tree with
  nothing focused all give `ErrNoActiveWindow`
- floating windows are found under `floating_nodes`, not `nodes`
- the server closes the connection mid-session: the next
  `ActiveWindow` reconnects to the same path and succeeds
- the server goes away and comes back at a *different* path, with
  `XDG_RUNTIME_DIR` pointed at `t.TempDir()`: the detector rediscovers
  it and succeeds. A test that only ever relistens on the original path
  would pass without the rediscovery existing at all
- two live sockets in the same directory: the detector refuses to guess
  and returns an error
- a stale socket *file* with no listener beside one live socket: the
  live one is adopted, because the probe drops the other
- a listener at a matching path answers `GET_VERSION` with garbage, or
  closes on it, or never replies: not adopted. The deadline case
  matters as much as the content one, since a socket that accepts and
  then says nothing must not wedge the poll loop
- `XDG_RUNTIME_DIR` unset: discovery does not run at all, whatever sits
  in `/tmp`
- a bad magic string and an implausible payload length are errors, not
  panics or a multi-gigabyte allocation
- `focusedWindow` and `swayAppName` get table tests, parallel, no
  socket

Socket paths have a 108-byte limit, so the tests use a one-character
basename under `t.TempDir()`, except where the name has to match sway's
glob, which is longer but still comfortably inside it.

The `SO_PEERCRED` check only gets its positive case. A test listener
runs as the test's own UID, and producing one that does not needs a
second user, so reading the function is the only cover its negative
path has. Keep it short and obvious.

Idle-side display resolution, in `idle_sway_linux_test.go` with
`XDG_RUNTIME_DIR` pointed at `t.TempDir()`:

- one live `wayland-1` socket present: the child's environment carries
  `WAYLAND_DISPLAY=wayland-1`
- `wayland-1` beside `wayland-1.lock`: the lock file is not a
  candidate, so the display still resolves
- a stale `wayland-1` socket file with no listener beside a live
  `wayland-2`: resolves to `wayland-2`. This is the crashed-compositor
  case, and without the connect probe it counts two candidates and
  resolves to nothing
- two *live* displays, or none at all: the inherited `WAYLAND_DISPLAY`
  stays, unguessed
- `XDG_RUNTIME_DIR` unset: no resolution attempted

`idle_sway_linux_test.go` also covers the state machine and the
lifecycle. The `newCmd` field lets a test hand the supervisor a `sh -c`
script instead of `swayidle`, so the process half runs for real with
nothing installed.

State machine, straight off an `io.Reader`:

- idle marker with a 5m timeout at a fixed clock: `IdleTime` reports
  5m, then 5m30s thirty seconds later
- resume marker returns it to zero
- unrecognised lines change nothing
- a truncated stream leaves the state it had

Effective timeout, table-driven:

- `5m` → `5m`; `1.5s` → `2s`; `500ms` → `1s`; `1s` → `1s`
- the value passed to `newCmd` is the effective one, and so is the
  backdating: a `1.5s` threshold reports `2s` at the idle event
- the bound on both sides: `2147483s` is accepted, `2147483s + 1ns`
  ceilings past it and is refused, `math.MaxInt64` is refused without
  overflowing on the way. A ceiling that wrapped would produce a small
  positive number and quietly pass

Supervisor, with a scripted fake:

- the argv built for a given timeout — `-w`, `-C /dev/null`, `timeout`,
  the seconds, the two marker commands. Both flags get their own
  assertion: drop `-C` and the daemon runs the user's lock and suspend
  commands, drop `-w` and the markers race, and no other test would
  catch either
- a fake that emits idle then exits: the detector restarts it, and
  `IdleTime` reads zero across the gap instead of holding the stale
  idle
- `Close` terminates the child and returns promptly; a second `Close`
  is safe
- `IdleTime` after `Close` reports zero instead of blocking

Selection, in `session_linux_test.go` and `idle_linux_test.go` with
`t.Setenv`, so not parallel:

- every row of the classification table, asserted against
  `swaySocketPath` and `waylandSession`
- `SWAYSOCK` alone classifies as Wayland, the partial-environment row
  that silently picked `xprintidle` before
- `XDG_SESSION_TYPE=wayland` alone is enough too
- `I3SOCK` set with nothing else: not Wayland, so `xprintidle` stays
- `WAYLAND_DISPLAY` set and no `swayidle` on `PATH`: `NopIdleDetector`,
  never `XIdleDetector`
- `WAYLAND_DISPLAY` set with no `SWAYSOCK`: `NewWindowDetector` gives
  `ErrUnsupportedPlatform`, never an `XWindowDetector`

## Out of Scope

Icons on Linux. `WindowInfo.AppIcon` stays nil there, as it is on X11
today. Filling it means resolving `app_id` to a `.desktop` file and
then through icon theme lookup, which is its own phase.

Hyprland, river, GNOME. Each has its own protocol for the focused
window — Hyprland's socket, GNOME's shell eval — and no shared one
worth building against yet. The idle half already works for any Wayland
compositor implementing `ext-idle-notify-v1`.

i3. It speaks the same IPC, so `SwayWindowDetector` would work against
`I3SOCK` unchanged, but i3 is X11 and `xdotool` already works there.
Wiring it up would buy nothing and cost `SWAYSOCK` its single meaning.

## Manual Verification

From inside a sway session:

```sh
mise run build-daemon
./trackkrd -debug
```

Watching the log: focus a native Wayland window (`foot`, `alacritty`)
and confirm the `app` field matches its `app_id` from `swaymsg -t
get_tree`. Focus an XWayland window and confirm it reports the
WM_CLASS name. Focus an empty workspace and confirm the segment closes.
Stop typing for longer than `idle_threshold` and confirm `entered idle
state` with an idle duration at least the threshold, then confirm
`resumed from idle` on the next keypress.

Two more need the daemon started the way it will actually run, as a
systemd user unit instead of from a terminal inside the session:

```sh
swaymsg reload      # config only: the socket stays, nothing should change
swaymsg exit        # then log back in: the socket path changes
```

After the second, check both halves recover, not just the visible one:

- the log shows the new `sway-ipc.<uid>.<pid>` path being adopted, and
  window records resume
- idle still works. Go idle past the threshold and come back, and
  confirm `entered idle state` and `resumed from idle` still appear.
  Window records resuming say nothing about `swayidle`, a separate
  child against a separate socket, and only this catches a display name
  that moved

Confirm too that no second lock or suspend fires on the idle timeout,
which is what `-C /dev/null` buys and what only a live session tests.

Compare `echo $SWAYSOCK` and `echo $WAYLAND_DISPLAY` in a fresh
terminal against what the daemon logged, so a recovery that worked by
luck, the names happening to be reused, is not mistaken for
rediscovery doing its job.
