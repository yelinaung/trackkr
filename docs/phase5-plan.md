# Phase 5: macOS Support

## Context

`trackkrd` runs on this Mac today, but only as a browser listener: every
start logs `window detection unavailable`, and the timeline shows tabs
with no application activity around them. Phase 5 gives macOS the same
window and idle detection Linux has had since Phase 2.

The interfaces survive unchanged: `WindowDetector` and `IdleDetector`
in `internal/tracker` keep their one-method shape, and the tracking
loop, the reporter, the queue, and the extension listener never learn
which platform they are on.

Their *construction* does change. A darwin detector needs the config
(whether to read titles, whether to prompt), a logger (to explain a
missing permission once), a trust checker, and a clock the tests can
control. Today `detectors.newWindow` takes no arguments and `run` loads
the config afterwards, so step 6 reorders that and touches
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
  com.trackkr.daemon.plist     # launchd user agent
  trackkr.app/                 # bundle skeleton: Info.plist, wrapper
scripts/
  bundle-macos.sh              # build, assemble the .app, ad-hoc sign
```

Modified: `window.go` (takes ownership of `ErrUnsupportedPlatform`),
`window_other.go` and `idle_other.go` (tags become
`!linux && !darwin`), `config.go` (macOS keys), `cmd/trackkrd/main.go`
and `main_test.go` (the detector factories take config), `mise.toml`
(a bundling task), `docs/plan.md`.

Every filename above matters. Go applies implicit build constraints
from `_darwin` suffixes, and `_test.go` files inherit them, so anything
named `*_darwin_test.go` never runs on the Linux CI runner. The
portable decision logic therefore lives in `titles.go`, with no suffix
at all.

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

This is the whole shape of the phase, so it comes first.

| Capability | API | Permission |
|---|---|---|
| Idle time | `CGEventSourceSecondsSinceLastEventType` | none |
| Frontmost app name and bundle id | `NSWorkspace.frontmostApplication` | none |
| Window title | Accessibility (`AXUIElement`) | Accessibility |
| Window title (alternative) | `CGWindowListCopyWindowInfo` | Screen Recording |

Two thirds of the value arrives with no prompt at all. Titles are the
expensive part, and `CGWindowListCopyWindowInfo` quietly omits
`kCGWindowName` rather than failing when the process lacks Screen
Recording -- a silent empty string is worse than a refusal, so the
Accessibility route is the one to take, since its trust state can be
queried directly.

The phase therefore lands in three stages, each useful on its own:
idle, then app names, then titles behind a permission the user grants
when they want them.

## Implementation Order

### Step 1: `idle_darwin.go` -- idle time, no permission

`CGEventSourceSecondsSinceLastEventType(kCGEventSourceStateHIDSystemState,
kCGAnyInputEventType)` returns seconds since the last input as a
`CFTimeInterval`. One cgo call, one framework (`CoreGraphics`), no
prompt, and it satisfies the existing `IdleDetector` interface as-is.

`NewIdleDetectorOrNop` on darwin returns the real detector rather than
the Nop, which is the first behavioural change the daemon will show:
walking away now ends the current record at the idle threshold, exactly
as on Linux.

### Step 2: `window_darwin.go` -- the frontmost application

`NSWorkspace.sharedWorkspace.frontmostApplication` gives
`localizedName`, `bundleIdentifier`, and `processIdentifier` with no
permission. Objective-C is not callable from cgo, so `macos_darwin.m`
provides the C boundary.

**The native contract.** Two operations, not one, because the title
lookup needs a pid and happens only when trusted:

```c
// Fills the caller's struct. Returns 0 on success, 1 when no
// application is frontmost. Strings are malloc'd UTF-8 and owned by
// the caller; both are NULL on failure.
typedef struct { char *name; char *bundleID; pid_t pid; } trackkr_app;
int trackkr_frontmost_app(trackkr_app *out);

// Returns a malloc'd UTF-8 title, or NULL when the application has no
// focused window, exposes no title, or the AX call fails. NULL is not
// an error the caller reports; it means "no title".
char *trackkr_focused_window_title(pid_t pid);
```

Both wrap their bodies in `@autoreleasepool`, since a daemon polling
every three seconds without one leaks steadily. Every `AXUIElementRef`
and `CFTypeRef` obtained from a `Copy` or `Create` call is released on
both the success and failure paths, and the Go side defers `C.free` on
every non-NULL string. `CFStringGetCString` with
`kCFStringEncodingUTF8` does the conversion, so a title with an em dash
or CJK survives.

**Which values cross the boundary.** `WindowInfo` has only `AppName`
and `Title`, and this phase does not add a field. The bundle identifier
is returned solely to decide whether anyone is present, and the pid
solely to look up a title; neither is stored or sent.

**`ErrNoActiveWindow`, deterministically.** The detector returns the
sentinel when `frontmostApplication` is nil, when the name is empty, or
when the bundle identifier is one of:

```
com.apple.loginwindow             the login or lock screen
com.apple.ScreenSaver.Engine      the screensaver
```

A rule that enumerates identifiers can be extended; "the loginwindow
process" cannot be implemented.

**A naming collision to decide now.** Linux reports `firefox` from
WM_CLASS; macOS reports `Firefox` from `localizedName` -- which is
exactly the string the browser extension already sends. The case
difference that keeps the two observations distinguishable on Linux
collapses on macOS, and Phase 6's deduplication was going to lean on
it.

Use the presence of a URL instead. An extension record always carries
one; a window record never does. That distinction is honest on both
platforms, survives the app being renamed, and needs no invented
suffix on the app name. Phase 6's note in `plan.md` should say so.

### Step 3: `titles.go` -- titles, and living without them

`AXIsProcessTrusted()` reports whether the process may read other
applications' UI. One cgo call; every decision around it is ordinary Go
in `titles.go`, which is what the Linux runner tests.

```go
// titlePolicy decides whether to attempt a title read. It caches the
// answer because AXIsProcessTrusted is a syscall and the poll runs
// every three seconds.
type titlePolicy struct {
    enabled   bool                 // macos_read_titles
    isTrusted func() bool          // the cgo call, or a fake
    now       func() time.Time     // injected, so tests need no sleeps
    log       func(trusted bool)   // called only on a transition

    mu        sync.Mutex
    checked   bool
    trusted   bool
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
- subsequent calls reuse the cached answer until `now()` passes
  `nextCheck`, two minutes later. Granting permission takes effect
  within that window with no restart, and a denied permission costs one
  syscall per two minutes rather than one per poll;
- `log` fires on transitions only -- untrusted-to-trusted and back --
  which answers "log once" precisely: once per untrusted period, not
  once per process. A permission revoked and re-granted says so both
  times;
- the mutex is not decoration. `ActiveWindow` is called from the
  tracker's single goroutine today, but a detector that silently
  requires that is a trap for the next caller.

The check is on demand inside `ActiveWindow`. A background goroutine
would need a lifecycle, a shutdown path, and a place in `run` to be
cancelled; a cached timestamp needs none of that.

**Degradation is explicit: an application is never lost to a title
failure.** When trusted, a nil title from `trackkr_focused_window_title`
-- no focused window, no title attribute, or an AX error -- yields the
app name with an empty title. The only thing that suppresses a record
entirely is `ErrNoActiveWindow`.

**The prompt is asynchronous.** `AXIsProcessTrustedWithOptions` with
`kAXTrustedCheckOptionPrompt` shows the dialog and returns the *current*
state immediately, which is false. The policy must not treat the call
as if it answers the question it asks: it prompts at most once per
process, keeps reporting untrusted, and picks up the grant at the next
recheck like any other.

### Step 4: build tags and the no-cgo path

`CGO_ENABLED=0 GOOS=darwin go build ./...` is how this repo checks
portability from the Linux runner, and cgo cannot cross-compile without
an SDK. Keeping that check working is fiddlier than one fallback file,
because four separate things break:

1. **Both factories disappear, not one.** `idle_darwin.go` imports C,
   so it is excluded without cgo, and `idle_other.go` no longer covers
   darwin once its tag becomes `!linux && !darwin`. That leaves
   `NewIdleDetectorOrNop` undefined, exactly as `NewWindowDetector`
   would be. One `platform_nocgo_darwin.go`
   (`//go:build darwin && !cgo`) supplies both.

2. **The sentinel goes with them.** `ErrUnsupportedPlatform` is
   currently declared in `window_other.go`, which stops covering darwin
   under the new tag -- so the no-cgo fallback would return a symbol
   that no longer exists. Move the declaration to `window.go`, which
   has no build constraint and already holds `ErrNoActiveWindow`. It
   belongs with the interface anyway.

3. **The Objective-C file is a build input.** The go tool refuses
   native source files when cgo is off, so an unconstrained `macos.m`
   fails the package load before any Go file is considered. It is named
   `macos_darwin.m` and carries `//go:build darwin && cgo`; the go tool
   honours build constraints in C and Objective-C files as it does in
   Go ones.

4. **A test file inherits its filename's constraint.** Anything ending
   `_darwin_test.go` is invisible to the Linux runner, which is why the
   portable logic and its tests live in `titles.go` and
   `titles_test.go`.

The real build on a Mac has cgo on by default and picks up the darwin
implementations without any flags.

### Step 5: the `.app` bundle and launchd

TCC attributes a permission grant to a code signature, a bundle
identifier, and a path. A bare binary launched from a terminal has no
identity of its own, so the grant lands on Terminal, or vanishes when
the binary is rebuilt somewhere else.

**Rebuilding changes identity, and the bundle does not fix that.**
Ad-hoc signing (`codesign -s -`) computes the identity from the binary,
so every rebuild produces a different one and macOS may ask for
Accessibility again. Two honest options, and the plan takes the first:

- create a self-signed code-signing certificate once in the login
  keychain and sign with it every time, giving a stable identity across
  rebuilds;
- sign ad-hoc and accept re-granting Accessibility after a rebuild.

`scripts/bundle-macos.sh` uses the certificate when
`TRACKKR_SIGN_IDENTITY` is set and falls back to ad-hoc with a printed
warning otherwise, so the tradeoff is visible at build time rather than
discovered when titles stop appearing.

**Installation contract.** Vague paths are how launchd agents fail
silently, so each is fixed:

| Thing | Path |
|---|---|
| Bundle | `~/Applications/trackkr.app` |
| Executable | `~/Applications/trackkr.app/Contents/MacOS/trackkrd` |
| Agent plist | `~/Library/LaunchAgents/com.trackkr.daemon.plist` |
| Config | `~/Library/Application Support/trackkr/config.toml` |
| Logs | `~/Library/Logs/trackkr/{daemon,daemon.err}.log` |

launchd runs the executable directly -- there is no wrapper script, and
`ProgramArguments[0]` is the absolute path above. `$HOME` cannot appear
in a plist, so `bundle-macos.sh` generates the file per user with the
paths expanded, rather than shipping a template that silently does
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

`-config` is passed explicitly even though `DefaultConfigPath()` would
resolve to the same place on macOS, because that function's comment
says `~/.config/trackkr/config.toml` while `os.UserConfigDir()` returns
`~/Library/Application Support` here. An explicit flag beats a
surprise, and the comment gets fixed in step 6.

Load and unload, for the README:

```
launchctl bootstrap gui/$UID ~/Library/LaunchAgents/com.trackkr.daemon.plist
launchctl bootout   gui/$UID/com.trackkr.daemon
```

`gui/$UID` rather than `system/`: the agent needs the logged-in
session to see a frontmost application at all.

### Step 6: config, and getting it to the detector

New client keys, defaulting to today's behaviour so an existing config
keeps working:

```toml
macos_read_titles = true                # false = app names only
macos_prompt_for_accessibility = false  # true = ask on first run
```

Exposing them is not enough: `cmd/trackkrd` holds
`newWindow func() (tracker.WindowDetector, error)`, a zero-argument
factory, so nothing the config says can reach the darwin
implementation. Both factories gain a config parameter:

```go
type detectors struct {
    newWindow func(*tracker.Config) (tracker.WindowDetector, error)
    newIdle   func(*tracker.Config, *zerolog.Logger) tracker.IdleDetector
}
```

Every implementation changes signature -- linux, darwin, the no-cgo
darwin fallback, the `!linux && !darwin` fallback -- along with
`platformDetectors()` and the fakes in `main_test.go`. Linux ignores
the config, which is fine; the alternative is a package-level setter,
and a hidden global is worse than an ignored parameter.

`DefaultConfigPath()`'s comment is corrected while the file is open: it
claims `~/.config/trackkr/config.toml`, but `os.UserConfigDir()` puts
it under `~/Library/Application Support` on macOS, which is the actual
path an operator needs.

`docs/plan.md` gets the Phase 5 status, the URL-based deduplication
note, and the macOS section of the daemon design.

## Testing

cgo code that talks to the window server cannot run on the Linux CI
runner, and mocking `NSWorkspace` proves nothing. The split that makes
this testable is the same one the extension uses: keep the cgo surface
to a single function that returns `(app, title string, err error)`, and
put every decision above it in ordinary Go.

That leaves these covered by tests in `titles_test.go` and
`config_test.go`, both of which the Linux runner executes:

- the trust cache: first call checks, the next ten reuse, and a call
  past `trustRecheckInterval` checks again, all driven by a fake
  `now()` with no sleeping;
- the untrusted-to-trusted transition mid-run, and back, asserting the
  log fires once per period rather than once per process or once per
  poll;
- `macos_read_titles = false` short-circuiting before `isTrusted` is
  called at all, which is what proves the config reaches the detector
  and that titles-off also means prompt-off;
- the prompt firing at most once per process while the state stays
  untrusted, since the API answers with the pre-prompt value;
- `ErrNoActiveWindow` for a nil application, an empty name, and each
  listed bundle identifier;
- a trusted read that yields no title keeping the application record.

What stays manual, on a Mac: the cgo calls themselves, the permission
prompt, the bundle's TCC identity, and whether a rebuild keeps the
Accessibility grant.

## Non-Goals

Screen Recording as a title fallback, per-window tracking beyond the
frontmost one, Spaces awareness, and a menu-bar UI. Wayland and Android
remain in Phase 6, as does deduplication -- this phase only makes sure
Phase 6 has a signal it can use.

## Verification

1. `mise test` and `mise test-race` pass; coverage stays at or above 50%.
2. `mise lint` clean, and `CGO_ENABLED=0 GOOS=darwin go build ./...`
   still compiles on the Linux runner -- the check that the no-cgo
   fallback, the relocated sentinel, and the constrained `.m` file all
   line up.
3. `mise run-daemon` on this Mac logs neither `window detection
   unavailable` nor `idle detection not implemented`.
4. Switch between applications and confirm records appear on the
   dashboard with app names beside the existing browser rows.
5. Without Accessibility granted: records carry app names and empty
   titles, and the log says so once, not every poll.
6. Grant Accessibility to the bundled app, wait for the re-check, and
   confirm titles start appearing without restarting the daemon.
7. Walk away past the idle threshold and confirm the record ends when
   you stopped, not when you came back.
8. `launchctl bootstrap gui/$UID …`, log out and back in, and confirm
   the daemon restarts and keeps its Accessibility grant.
9. Rebuild, re-sign, and re-bundle, then confirm whether the grant
   survives. With `TRACKKR_SIGN_IDENTITY` set it should; ad-hoc is the
   case that may ask again, and the answer belongs in the README
   rather than in a surprise.
