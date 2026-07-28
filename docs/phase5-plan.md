# Phase 5: macOS Support

## Context

`trackkrd` runs on this Mac today as a browser listener and nothing
more. Every start logs `window detection unavailable`, and the timeline
shows tabs floating with no application activity around them. Phase 5
gives macOS the window and idle detection Linux has had since Phase 2.

The interfaces survive unchanged. `WindowDetector` and `IdleDetector`
in `internal/tracker` keep their one-method shape, and the tracking
loop, the reporter, the queue, and the extension listener never learn
which platform they are on.

Their construction changes. A darwin detector needs the config (whether
to read titles, whether to prompt), a logger (to explain a missing
permission once), a trust checker, and a clock the tests can control.
Today `detectors.newWindow` takes no arguments and `run` loads the
config afterwards, so step 6 reorders that and touches
`cmd/trackkrd/main.go`, `main_test.go`, `config.go`, and
`config_test.go`.

## New Files

```
internal/tracker/
  titles.go                    # portable: trust state, recheck interval
  titles_test.go               # no suffix, so Linux CI runs it
  window_darwin.go             # //go:build darwin && cgo
  idle_darwin.go               # //go:build darwin && cgo
  macos_darwin.m               # //go:build darwin && cgo
  platform_nocgo_darwin.go     # //go:build darwin && !cgo, both factories
deploy/
  Info.plist                   # bundle template: id, LSUIElement
  README-macos.md              # install, permissions, launchctl, signing
scripts/
  bundle-macos.sh              # build, assemble the .app, sign, emit the plist
```

No wrapper script and no checked-in plist appear here. launchd runs the
bundled binary directly, and the agent plist is generated per user
because `$HOME` cannot appear in one.

Modified: `window.go` (takes ownership of `ErrUnsupportedPlatform`),
`window_other.go` and `idle_other.go` (tags become `!linux && !darwin`),
`config.go` and `config_test.go` (macOS keys, and the
`DefaultConfigPath` comment that misdescribes macOS),
`cmd/trackkrd/main.go` and `main_test.go` (factories take config and
logger), `mise.toml` (a bundling task), `docs/plan.md`.

Every filename above matters. Go applies implicit build constraints
from `_darwin` suffixes, and `_test.go` files inherit them, so anything
named `*_darwin_test.go` never runs on the Linux CI runner. The portable
decision logic therefore lives in `titles.go`, with no suffix at all.

Four build combinations have to compile, and each needs exactly one
definition of each factory:

| Build | `NewWindowDetector` | `NewIdleDetectorOrNop` | Native |
|---|---|---|---|
| `linux` | `window_linux.go` | `idle_linux.go` | none |
| `darwin && cgo` | `window_darwin.go` | `idle_darwin.go` | `macos_darwin.m` |
| `darwin && !cgo` | `platform_nocgo_darwin.go` | `platform_nocgo_darwin.go` | excluded |
| `!linux && !darwin` | `window_other.go` | `idle_other.go` | none |

`ErrUnsupportedPlatform` sits in `window.go` with no constraint at all,
so every row can return it.

## What Needs Permission, And What Does Not

| Capability | API | Permission |
|---|---|---|
| Idle time | `CGEventSourceSecondsSinceLastEventType` | none |
| Frontmost app name and bundle id | `NSWorkspace.frontmostApplication` | none |
| Window title | Accessibility (`AXUIElement`) | Accessibility |
| Window title (alternative) | `CGWindowListCopyWindowInfo` | Screen Recording |

Two thirds of the value arrives with no prompt at all. Titles cost more.
`CGWindowListCopyWindowInfo` omits `kCGWindowName` silently when the
process lacks Screen Recording, handing back an empty string that looks
like data; Accessibility refuses openly and exposes its trust state to a
direct query. Phase 5 takes the Accessibility route.

The phase lands in three stages, each useful alone: idle, then app
names, then titles behind a permission the user grants when they want
them.

## Implementation Order

### Step 1: `idle_darwin.go` -- idle time, no permission

`CGEventSourceSecondsSinceLastEventType(kCGEventSourceStateHIDSystemState,
kCGAnyInputEventType)` returns seconds since the last input as a
`CFTimeInterval`. One cgo call, one framework (`CoreGraphics`), no
prompt, and it satisfies the existing `IdleDetector` interface as it
stands.

`NewIdleDetectorOrNop` on darwin returns the real detector instead of
the Nop, the first behavioural change the daemon will show: walking away
now ends the current record at the idle threshold, as it does on Linux.

### Step 2: `window_darwin.go` -- the frontmost application

`NSWorkspace.sharedWorkspace.frontmostApplication` gives
`localizedName`, `bundleIdentifier`, and `processIdentifier` with no
permission. Objective-C is not callable from cgo, so `macos_darwin.m`
provides the C boundary.

**The native contract.** The boundary carries two operations, because
the title lookup needs a pid and happens only when trusted:

```c
typedef struct { char *name; char *bundleID; pid_t pid; } trackkr_app;

#define TRACKKR_OK        0  // out is populated
#define TRACKKR_NO_APP    1  // nobody is frontmost: ErrNoActiveWindow
#define TRACKKR_FAILED    2  // allocation or conversion failed: a real error

// Zero-initializes *out before doing anything. On any non-OK return,
// every partial allocation is freed and both pointers are NULL, so the
// caller never frees a dangling pointer and never mistakes a failed
// conversion for an empty desktop.
int trackkr_frontmost_app(trackkr_app *out);

// Returns a malloc'd UTF-8 title, or NULL when the application has no
// focused window, exposes no title, the AX call fails, or the call
// times out. NULL is not an error; it means "no title".
//
// Calls AXUIElementSetMessagingTimeout(app, 0.5) before reading any
// attribute.
char *trackkr_focused_window_title(pid_t pid);
```

Three statuses earn their keep. "Nobody is frontmost" is an ordinary
state that maps to `ErrNoActiveWindow`, while a failed
`CFStringGetCString` is a fault worth surfacing. Two statuses would
report an empty desktop every time a conversion failed.

**The AX timeout is a shutdown requirement.** The tracker calls
`ActiveWindow` synchronously from its only goroutine, and no cgo call
can be interrupted by context cancellation. An application that stops
answering Accessibility messages -- a beachballing app, most often --
stalls the poll loop. `Run` then cannot return on `ctx.Done()`, and the
daemon refuses to shut down with its final record unwritten.

`AXUIElementSetMessagingTimeout(app, 0.5)` bounds the call. Half a
second sits well inside the three-second poll interval: a hung
application costs a fraction of one tick, and a busy-but-alive one still
answers. A timeout returns NULL and degrades like any other missing
title, so the application record survives with an empty title.

The Go side checks `ctx.Err()` before entering cgo, so a cancelled
context skips the call instead of starting one nothing can stop.
Wrapping the call in a goroutine and selecting on the context looks like
the obvious alternative and is worse: the goroutine stays blocked in cgo
holding an OS thread, and an unresponsive application leaks one per
poll.

Both functions wrap their bodies in `@autoreleasepool`, since a daemon
polling every three seconds without one leaks steadily. Every
`AXUIElementRef` and `CFTypeRef` from a `Copy` or `Create` call is
released on the success and failure paths alike, and the Go side defers
`C.free` on every non-NULL string. `CFStringGetCString` with
`kCFStringEncodingUTF8` does the conversion, so a title with an em dash
or CJK survives.

**Which values cross the boundary.** `WindowInfo` has only `AppName` and
`Title`, and this phase adds no field. The bundle identifier decides
whether anyone is present; the pid looks up a title. Neither is stored
or sent.

**`ErrNoActiveWindow`, deterministically.** The detector returns the
sentinel when `frontmostApplication` is nil, when the name is empty, or
when the bundle identifier is one of:

```
com.apple.loginwindow             the login or lock screen
com.apple.ScreenSaver.Engine      the screensaver
```

Someone can extend a list of identifiers. Nobody can implement "the
loginwindow process".

**A naming collision to decide now.** Linux reports `firefox` from
WM_CLASS; macOS reports `Firefox` from `localizedName`, the string the
browser extension already sends. The case difference that keeps the two
observations apart on Linux collapses here, and Phase 6's deduplication
was going to lean on it.

Use the presence of a URL. An extension record always carries one; a
window record never does. The distinction holds on both platforms,
survives an application being renamed, and needs no invented suffix.
Phase 6's note in `plan.md` should say so.

### Step 3: `titles.go` -- titles, and living without them

`AXIsProcessTrusted()` reports whether the process may read other
applications' UI. One cgo call; every decision around it is ordinary Go
in `titles.go`, which is what the Linux runner tests.

```go
// trustState is tri-valued on purpose: the zero value is "not yet
// asked", so the first observation is distinguishable from a cached
// untrusted answer and can be logged.
type trustState int

const (
    trustUnknown trustState = iota
    trustGranted
    trustDenied
)

// titlePolicy decides whether to attempt a title read. It caches the
// answer because AXIsProcessTrusted is a syscall and the poll runs
// every three seconds.
type titlePolicy struct {
    enabled       bool               // macos_read_titles
    promptEnabled bool               // macos_prompt_for_accessibility
    isTrusted     func() bool        // the cgo call, or a fake
    prompt        func()             // AXIsProcessTrustedWithOptions, or a fake
    now           func() time.Time   // injected, so tests need no sleeps
    log           func(trusted bool) // first observation, then changes only

    mu        sync.Mutex
    state     trustState
    prompted  bool
    nextCheck time.Time
}

const trustRecheckInterval = 2 * time.Minute
```

The rules, each one a test:

- `enabled == false` short-circuits before `isTrusted` is ever called,
  so `macos_read_titles = false` also means no prompt, whatever
  `macos_prompt_for_accessibility` says. Turning titles off turns the
  Accessibility machinery off entirely;
- the first call checks and caches, so startup costs one syscall;
- later calls reuse the cached answer until `now()` passes `nextCheck`,
  two minutes on. Granting permission takes effect inside that window
  with no restart, and a denied permission costs one syscall every two
  minutes instead of one per poll;
- `log` fires on the first observation and on every change after it. A
  two-valued cache cannot manage this, because an initial `false` looks
  identical to a cached `false`, and the warning an operator needs at
  startup never appears. `trustUnknown` as the zero value makes "not yet
  asked" a state instead of an accident. "Log once" therefore means once
  per untrusted period, and a permission revoked and re-granted says so
  both times;
- the prompt stays separate from the check. On the first denied
  observation, when `promptEnabled` holds and `prompted` does not, call
  `prompt()`, set `prompted`, and keep the denied answer -- the API
  returns the pre-prompt value, so believing it would be wrong. Any
  later grant arrives through the ordinary recheck. Without `prompted`
  in the struct, "at most once per process" has no expression and no
  test;
- the mutex is not decoration. `ActiveWindow` runs on the tracker's
  single goroutine today, and a detector that silently depends on that
  is a trap for the next caller;
- **no callback runs while `mu` is held.** The locked section decides
  the new state, reserves the prompt by setting `prompted`, and records
  what needs announcing; `prompt` and `log` fire after the unlock.
  Injected callbacks are arbitrary code -- a logger that reaches back
  into the detector, or a test double that does -- and calling them
  under the lock invites a deadlock for nothing. Reserving under the
  lock keeps one-check and one-prompt intact anyway.

The check happens on demand inside `ActiveWindow`. A background
goroutine would need a lifecycle, a shutdown path, and a place in `run`
to be cancelled; a cached timestamp needs none of that.

**Degradation is explicit: a title failure never loses an application.**
When trusted, a nil title from `trackkr_focused_window_title` -- no
focused window, no title attribute, or an AX error -- yields the app
name with an empty title. Only `ErrNoActiveWindow` suppresses a record.

**The prompt is asynchronous.** `AXIsProcessTrustedWithOptions` with
`kAXTrustedCheckOptionPrompt` shows the dialog and returns the current
state immediately, which is false. The policy must not treat the call as
if it answers the question it asks: it prompts at most once per process,
keeps reporting untrusted, and picks the grant up at the next recheck
like any other.

### Step 4: build tags and the no-cgo path

`CGO_ENABLED=0 GOOS=darwin go build ./...` is how this repo checks
portability from the Linux runner, and cgo cannot cross-compile without
an SDK. Keeping that check working takes more than one fallback file,
because four things break at once:

1. **Both factories disappear.** `idle_darwin.go` imports C, so cgo-off
   excludes it, and `idle_other.go` stops covering darwin once its tag
   becomes `!linux && !darwin`. `NewIdleDetectorOrNop` goes undefined
   exactly as `NewWindowDetector` would. One `platform_nocgo_darwin.go`
   (`//go:build darwin && !cgo`) supplies both.

2. **The sentinel goes with them.** `ErrUnsupportedPlatform` currently
   lives in `window_other.go`, which stops covering darwin under the new
   tag, so the no-cgo fallback would return a symbol that no longer
   exists. Move the declaration to `window.go`, which carries no build
   constraint and already holds `ErrNoActiveWindow`. It belongs with the
   interface anyway.

3. **The Objective-C file is a build input.** The go tool refuses native
   source files when cgo is off, so an unconstrained `macos.m` fails the
   package load before any Go file is read. The file is named
   `macos_darwin.m` and carries `//go:build darwin && cgo`; the go tool
   honours build constraints in C and Objective-C files as it does in Go
   ones.

4. **A test file inherits its filename's constraint.** Anything ending
   `_darwin_test.go` is invisible to the Linux runner, which is why the
   portable logic and its tests live in `titles.go` and `titles_test.go`.

A build on a Mac has cgo on by default and picks up the darwin
implementations with no flags.

### Step 5: the `.app` bundle and launchd

TCC attributes a permission grant to a code signature, a bundle
identifier, and a path. A bare binary launched from a terminal has no
identity of its own, so the grant lands on Terminal, or vanishes when
the binary is rebuilt somewhere else.

**Rebuilding changes identity, and the bundle does not fix that.**
Ad-hoc signing (`codesign -s -`) computes the identity from the binary,
so every rebuild produces a new one and macOS may ask for Accessibility
again. Two honest options exist, and the plan takes the first:

- create a self-signed code-signing certificate once in the login
  keychain and sign with it every time, for a stable identity across
  rebuilds;
- sign ad-hoc and accept re-granting Accessibility after a rebuild.

`scripts/bundle-macos.sh` uses the certificate when
`TRACKKR_SIGN_IDENTITY` is set and falls back to ad-hoc with a printed
warning, so the tradeoff shows up at build time and not when titles stop
appearing.

The script installs, and does not merely build. It creates
`~/Applications`, `~/Library/LaunchAgents`, `~/Library/Logs/trackkr`,
and the config directory when absent, assembles the bundle at the fixed
path below, signs it, and writes the generated agent plist. An artifact
left in `dist/` would carry a different code identity from the one macOS
granted Accessibility to.

**Installation contract.** Vague paths are how launchd agents fail
silently, so each one is fixed:

| Thing | Path |
|---|---|
| Bundle | `~/Applications/trackkr.app` |
| Executable | `~/Applications/trackkr.app/Contents/MacOS/trackkrd` |
| Agent plist | `~/Library/LaunchAgents/com.trackkr.daemon.plist` |
| Config | `~/Library/Application Support/trackkr/config.toml` |
| Logs | `~/Library/Logs/trackkr/{daemon,daemon.err}.log` |

launchd runs the executable directly, with no wrapper script, and
`ProgramArguments[0]` holds the absolute path above. `$HOME` cannot
appear in a plist, so `bundle-macos.sh` generates the file per user with
the paths expanded, instead of shipping a template that silently does
nothing.

```xml
<key>ProgramArguments</key>
<array>
  <string>/Users/NAME/Applications/trackkr.app/Contents/MacOS/trackkrd</string>
  <string>-config</string>
  <string>/Users/NAME/Library/Application Support/trackkr/config.toml</string>
</array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
<key>StandardOutPath</key><string>/Users/NAME/Library/Logs/trackkr/daemon.log</string>
<key>StandardErrorPath</key><string>/Users/NAME/Library/Logs/trackkr/daemon.err.log</string>
```

The plist passes `-config` explicitly even though `DefaultConfigPath()`
resolves to the same place on macOS, because that function's comment
says `~/.config/trackkr/config.toml` while `os.UserConfigDir()` returns
`~/Library/Application Support` here. An explicit flag beats a surprise,
and step 6 fixes the comment.

The README carries the load and unload commands:

```
launchctl bootstrap gui/$UID ~/Library/LaunchAgents/com.trackkr.daemon.plist
launchctl bootout   gui/$UID/com.trackkr.daemon
```

`gui/$UID` and not `system/`: the agent needs the logged-in session to
see a frontmost application at all.

### Step 6: config, and getting it to the detector

New client keys. Their defaults describe the behaviour this phase
introduces, since macOS has no window detector today and nothing to
preserve:

```toml
macos_read_titles = true                # false = app names only, no AX at all
macos_prompt_for_accessibility = false  # true = ask once on first denial
```

Titles default on because they make a record useful, and leaving them on
without permission costs an empty string. The prompt defaults off
because a background agent throwing a system dialog at an operator who
never asked for one is an unpleasant surprise; `mise` output and the
README point at the setting.

Exposing the keys is not enough. `cmd/trackkrd` holds
`newWindow func() (tracker.WindowDetector, error)`, a zero-argument
factory, so nothing the config says can reach the darwin implementation.
Both factories gain parameters:

```go
type detectors struct {
    newWindow func(*tracker.Config, *zerolog.Logger) (tracker.WindowDetector, error)
    newIdle   func(*tracker.Config, *zerolog.Logger) tracker.IdleDetector
}
```

The window factory takes the logger too. The darwin detector has to
explain a missing Accessibility permission exactly once, and `run`
already holds both values; passing one and not the other would send the
detector reaching for a package-level logger, the global this codebase
has avoided everywhere else. The trust checker and the clock stay
private constructor seams with production defaults, supplied only by
tests.

Every implementation changes signature -- linux, darwin, the no-cgo
darwin fallback, the `!linux && !darwin` fallback -- along with
`platformDetectors()` and the fakes in `main_test.go`. Linux ignores the
config, which is fine. The alternative is a package-level setter, and a
hidden global is worse than an ignored parameter.

`DefaultConfigPath()`'s comment gets corrected while the file is open:
it claims `~/.config/trackkr/config.toml`, but `os.UserConfigDir()` puts
it under `~/Library/Application Support` on macOS, which is the path an
operator actually needs.

`docs/plan.md` gets the Phase 5 status, the URL-based deduplication
note, and the macOS section of the daemon design.

## Testing

cgo code that talks to the window server cannot run on the Linux CI
runner, and mocking `NSWorkspace` proves nothing. Three layers keep the
untestable part small:

```go
// Portable, in titles.go. Every decision lives here.
type appInfo struct {
    Name     string
    BundleID string
    PID      int
}

type detectorCore struct {
    policy    *titlePolicy
    frontmost func() (*appInfo, error) // darwin: one C call
    titleFor  func(pid int) string     // darwin: the other C call
}

func (d *detectorCore) ActiveWindow(context.Context) (WindowInfo, error)

// mapFrontmost turns a native status into the seam's contract. Pure,
// so a switch typo in the adapter is caught on Linux and not only on a
// Mac with an empty desktop.
func mapFrontmost(status int, app appInfo) (*appInfo, error) {
    switch status {
    case statusOK:
        return &app, nil
    case statusNoApp:
        return nil, nil   // the core maps this to ErrNoActiveWindow
    default:
        return nil, errFrontmostFailed
    }
}
```

`frontmost` returns a pointer. Three native statuses need three Go
outcomes, and a value type collapses two of them: `TRACKKR_NO_APP` has
to be expressible as `(nil, nil)`, distinct from an operational failure
carrying an error and from a real application. A zero-valued `appInfo`
for "nobody is frontmost" would force the core to infer emptiness from a
blank name, the guessing the three statuses exist to remove.

`window_darwin.go` builds a `detectorCore` from the two C calls wrapped
as Go functions, and does nothing else. Which bundle identifiers mean
`ErrNoActiveWindow`, whether to consult the policy, what to do when a
trusted read yields no title -- all of it is ordinary Go that the Linux
runner executes with fakes for both seams.

`titles_test.go` and `config_test.go` run in CI and cover:

- the trust cache: the first call checks, the next ten reuse, and a call
  past `trustRecheckInterval` checks again, all driven by a fake `now()`
  with no sleeping;
- the first observation logging even though nothing changed, then
  transitions in both directions logging once each;
- the prompt firing at most once per process, the denied answer
  retained, and no prompt at all when `promptEnabled` is false;
- `macos_read_titles = false` short-circuiting before `isTrusted` or
  `prompt` is called, which proves the config reaches the detector and
  that titles-off means prompt-off;
- `mapFrontmost` over all three statuses, so an adapter switch typo
  fails on Linux;
- `ErrNoActiveWindow` for a nil application, an empty name, and each
  listed bundle identifier, through the injected `frontmost`;
- a trusted read whose `titleFor` returns "" keeping the application
  record with an empty title;
- an operational failure from `frontmost` surfacing as an error and not
  as an empty desktop;
- a cancelled context returning before `titleFor` is called, so shutdown
  never waits on a native call;
- the config keys and their defaults.

A daemon test in `cmd/trackkrd` asserts that the loaded config and the
logger both reach the factory, since a factory that silently ignores its
arguments would pass every test above.

Four things stay manual, on a Mac: the cgo calls themselves, the
permission prompt, the bundle's TCC identity, and whether a rebuild
keeps the Accessibility grant.

## Non-Goals

Screen Recording as a title fallback, per-window tracking beyond the
frontmost one, Spaces awareness, and a menu-bar UI. Wayland and Android
stay in Phase 6, as does deduplication -- this phase only makes sure
Phase 6 has a signal it can use.

## Verification

1. `mise test` and `mise test-race` pass; coverage stays at or above 50%.
2. `mise lint` clean, and on the Linux runner both
   `CGO_ENABLED=0 GOOS=darwin go build ./...` (the no-cgo fallback, the
   relocated sentinel, and the constrained `.m` file) and
   `CGO_ENABLED=0 GOOS=windows go build ./...` (the `!linux && !darwin`
   row, which nothing else exercises) compile.
3. On a Mac: `go build ./...` with cgo on, and `mise bundle-macos`
   produces a signed bundle. Neither runs in CI, so both belong in the
   phase's checklist and not in its assumptions.
4. `mise run-daemon` on this Mac logs neither `window detection
   unavailable` nor `idle detection not implemented`.
5. Switch between applications and confirm records appear on the
   dashboard with app names beside the existing browser rows.
6. Without Accessibility granted: records carry app names and empty
   titles, and the log says so once, not every poll.
7. Grant Accessibility to the bundled app, wait for the recheck, and
   confirm titles start appearing with no restart.
8. Walk away past the idle threshold and confirm the record ends when
   you stopped, not when you came back.
9. `launchctl bootstrap gui/$UID …`, log out and back in, and confirm
   the daemon restarts and keeps its Accessibility grant.
10. Rebuild, re-sign, and re-bundle, then confirm whether the grant
    survives. With `TRACKKR_SIGN_IDENTITY` set it should; ad-hoc is the
    case that may ask again, and the README should say which.
