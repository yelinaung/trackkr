# Phase 5: macOS Support

## Context

`trackkrd` runs on this Mac today, but only as a browser listener: every
start logs `window detection unavailable`, and the timeline shows tabs
with no application activity around them. Phase 5 gives macOS the same
window and idle detection Linux has had since Phase 2.

The interfaces are already in place. `WindowDetector` and
`IdleDetector` in `internal/tracker` have Linux implementations behind
`//go:build linux` and a `!linux` fallback that returns
`ErrUnsupportedPlatform`. Nothing above them changes: the tracking loop,
the reporter, the queue, and the extension listener are platform-blind.

## New Files

```
internal/tracker/
  window_darwin.go           # frontmost app + title, cgo
  window_darwin_test.go
  idle_darwin.go             # CGEventSource idle, cgo
  idle_darwin_test.go
  macos.m                    # the Objective-C cgo needs for NSWorkspace
  permissions_darwin.go      # Accessibility trust state, degradation
  permissions_darwin_test.go
  window_nocgo_darwin.go     # darwin && !cgo fallback
deploy/
  com.trackkr.daemon.plist   # launchd user agent
  trackkr.app/               # bundle skeleton: Info.plist, wrapper
scripts/
  bundle-macos.sh            # build, assemble the .app, ad-hoc sign
```

Modified: `window_other.go` and `idle_other.go` (build tags become
`!linux && !darwin`), `mise.toml` (a bundling task), `docs/plan.md`.

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
`localizedName` and `bundleIdentifier` with no permission. Objective-C
is not callable from cgo directly, so `macos.m` holds a small C
function that returns both as C strings and the Go side owns the
freeing.

Returning `ErrNoActiveWindow` matters here as much as on Linux: a
locked screen, a Space transition, or the loginwindow process all mean
"nobody is looking at anything", and the tracker already knows what to
do with that sentinel.

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

### Step 3: `permissions_darwin.go` -- titles, and living without them

`AXIsProcessTrusted()` reports whether the process may read other
applications' UI. The detector checks it, and:

- trusted: read `kAXFocusedWindowAttribute` then `kAXTitleAttribute`
  from `AXUIElementCreateApplication(pid)`, and report app plus title;
- not trusted: report the app with an empty title, and log the reason
  once rather than on every three-second poll.

Re-check periodically -- every few minutes, not every poll -- so that
granting permission takes effect without restarting the daemon. A
config key (`macos_prompt_for_accessibility`, default false) decides
whether the first check uses `AXIsProcessTrustedWithOptions` with the
prompt, since a background agent throwing up a system dialog uninvited
is rude.

Empty titles are not a failure. The dashboard already shows app-level
rows, and a title-less `Ghostty` record is far more useful than no
record at all.

### Step 4: build tags and the no-cgo path

`window_other.go` and `idle_other.go` become `!linux && !darwin`.

cgo is the wrinkle for CI. The Linux runner cross-compiles with
`GOOS=linux` today and `GOOS=darwin go build ./...` is part of how this
repo checks portability, but cgo cannot cross-compile without an SDK.
So `window_nocgo_darwin.go` carries `//go:build darwin && !cgo` and
returns `ErrUnsupportedPlatform`, letting `CGO_ENABLED=0 GOOS=darwin`
compile the tree exactly as it does now. The real build on a Mac has
cgo on by default.

### Step 5: the `.app` bundle and launchd

TCC attributes a permission grant to a code signature, a bundle
identifier, and a path. A bare binary launched from a terminal has no
identity of its own, so the grant lands on Terminal -- or disappears
when the binary is rebuilt to a new path. Accessibility permission
therefore needs the daemon inside a signed bundle, not because of
anything about the code.

`scripts/bundle-macos.sh` builds `trackkrd`, assembles
`trackkr.app/Contents/MacOS/trackkrd` with an `Info.plist` carrying
`CFBundleIdentifier = com.trackkr.daemon` and `LSUIElement = true` (no
Dock icon), and ad-hoc signs it (`codesign -s -`). Ad-hoc is enough for
a personal machine; a Developer ID matters only for distribution.

`com.trackkr.daemon.plist` is a launchd *user* agent, not a system
daemon: it needs the logged-in user's session to see the frontmost
application at all.

### Step 6: config and docs

New client keys, defaulting to today's behaviour so an existing config
keeps working:

```toml
macos_read_titles = true                # false = app names only
macos_prompt_for_accessibility = false  # true = ask on first run
```

`docs/plan.md` gets the Phase 5 status, the URL-based deduplication
note, and the macOS section of the daemon design.

## Testing

cgo code that talks to the window server cannot run on the Linux CI
runner, and mocking `NSWorkspace` proves nothing. The split that makes
this testable is the same one the extension uses: keep the cgo surface
to a single function that returns `(app, title string, err error)`, and
put every decision above it in ordinary Go.

That leaves these covered by tests, on any platform:

- the degradation state machine: trusted, not trusted, and the
  transition when permission is granted mid-run, driven through an
  injected trust checker;
- the re-check interval, so a granted permission is noticed without a
  restart and without one syscall per poll;
- `ErrNoActiveWindow` mapping for an empty app name;
- the config keys and their defaults.

What stays manual, on a Mac: the cgo calls themselves, the permission
prompt, and the bundle's TCC identity.

## Non-Goals

Screen Recording as a title fallback, per-window tracking beyond the
frontmost one, Spaces awareness, and a menu-bar UI. Wayland and Android
remain in Phase 6, as does deduplication -- this phase only makes sure
Phase 6 has a signal it can use.

## Verification

1. `mise test` and `mise test-race` pass; coverage stays at or above 50%.
2. `mise lint` clean, and `CGO_ENABLED=0 GOOS=darwin go build ./...`
   still compiles on the Linux runner.
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
8. Load the launchd agent, log out and back in, and confirm the daemon
   restarts and keeps its Accessibility grant.
