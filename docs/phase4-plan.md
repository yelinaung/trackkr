# Phase 4: Firefox Extension

## Context

The daemon records which window has focus. When that window is Firefox it
records "firefox" and a page title, which is a poor description of a day
spent across thirty tabs. Phase 4 adds a WebExtension that reports the
active tab to the daemon, which enriches it and feeds it through the same
reporter queue the desktop tracker uses.

Nothing about the server changes: the daemon still posts to
`POST /api/v1/activity` with its device key, so extension traffic arrives as
that device's activity with a `url` attached.

## New Files

```
internal/tracker/
  extension.go               # localhost listener + handler
  extension_test.go
extension/
  manifest.json              # MV3, Firefox
  background.js              # tab listeners, idle, queue, delivery
  popup.html                 # connection status
  popup.js
  popup.css
  options.html               # daemon URL, token, permission request
  options.js
  options.css
  icons/
```

Modified: `internal/tracker/config.go` and `config_test.go` (listener
address, token, enable flag, loopback validation), `cmd/trackkrd/main.go`
and `main_test.go` (the `-print-extension-token` flag, starting and
stopping the listener), `docs/plan.md` (record the MV3 and auth
decisions).

## Deviations From `plan.md`

Two things in the original sketch do not survive contact.

**Manifest V2 becomes V3.** Firefox has supported MV3 since 109 and new
AMO submissions target it; MV2 is on a deprecation path. Two consequences
shape `background.js`.

The background script becomes a non-persistent event page, so it cannot
hold the "current tab" in a module variable and trust it to survive.

More importantly, Firefox treats MV3 `host_permissions` as *optional*:
unlike Chrome it does not grant them at install, the user can decline or
later revoke them in `about:addons`, and older versions did not even
prompt. A missing permission makes every fetch fail with an opaque
`NetworkError` that is indistinguishable from "daemon down" -- the most
likely state of a fresh install, and undiagnosable without help. So the
extension checks `browser.permissions.contains()` before reporting, and
the popup carries a dedicated state for it (step 4).

**"No auth needed, localhost only" is not enough.** Any process on the
machine can POST to a localhost port, and so can any web page the user
visits: a form post or a `text/plain` fetch is a simple CORS request that
the browser sends without a preflight. The response is unreadable to the
attacker, but the write already happened, so a visited page could inject
fabricated browsing history into the user's own timeline. The listener
therefore requires all of:

- a bearer token (step 2 covers where it comes from), pasted into the
  extension's options page and compared with `hmac.Equal`;
- `Content-Type: application/json`, which forces a preflight for any
  cross-origin caller, and the daemon answers no preflight;
- an `Origin` that is absent or `moz-extension://…` -- never `http(s)://`.

Binding to `127.0.0.1` is assumed throughout, not `0.0.0.0`: the port must
not be reachable from the network at all.

## Implementation Order

### Step 1: `extension.go` -- the daemon's localhost listener

`POST /extension/activity` takes a batch, mirroring the server's own
ingest shape, so draining a backlog after an outage costs one request
rather than one per record:

```json
{"records": [
  {"url": "https://…", "title": "…", "started_at": "…", "ended_at": "…"}
]}
```

The daemon computes `duration_s` itself rather than trusting the caller,
sets `app_name` to `Firefox`, and enqueues through the existing
`Reporter`, so persistence, batching, and retry come for free.

Validation, each with a test, with the numbers stated so two implementors
build the same thing:

- method `POST`, and `Content-Type` parsed with `mime.ParseMediaType` so
  `application/json; charset=utf-8` is accepted rather than 415'd;
- token present and equal;
- `Origin` absent or `moz-extension://…`;
- JSON shape;
- duration `< 1s` -- dropped, matching the extension-side rule;
- duration `> 12h` (`maxRecordDuration`) -- **clamped** to
  `started_at + 12h`, not dropped. A suspended laptop produces one
  enormous segment, and discarding it would throw away the real browsing
  that preceded the sleep along with the phantom hours;
- URL scheme not `http`/`https` -- dropped, so `about:`,
  `moz-extension:`, and `file:` never reach the server.

`app_name` is `Firefox` while the desktop tracker reports `firefox` from
WM_CLASS. That is deliberate: they are different observations of the same
time, and collapsing them would hide the double-count described under
Known Overlap. It does mean per-app aggregation shows both, which is the
honest picture until Phase 6 deduplicates.

`GET /extension/status` returns `{"ok": true}` for the popup, behind the
same token, so the popup can distinguish "daemon down" from "wrong token".

The listener is opt-in: `extension_enabled = false` by default, since a
daemon on a headless box has no browser talking to it.

### Step 2: config and wiring

New client config keys, with env overrides matching the existing pattern:

```toml
extension_enabled = true
extension_addr    = "127.0.0.1:7600"
extension_token   = ""   # or TRACKKR_EXTENSION_TOKEN
```

Where the token comes from has to be stated, because "generated by the
daemon" and "refuse to start without one" cannot both be true: a daemon
that will not start can never generate anything. So generation is its own
command:

```
trackkrd -print-extension-token
```

It prints 32 random bytes as hex to stdout and exits, touching neither the
config file nor the database. The token does not rotate: it is whatever
the config says, so restarting the daemon never invalidates what is
already pasted into the extension. The user pastes it into `extension_token` (or exports
`TRACKKR_EXTENSION_TOKEN`) and into the extension's options page. The
daemon never writes the config file itself -- rewriting TOML would
discard the user's comments and formatting.

`Validate` then rejects an enabled listener with an empty token, and
rejects an address that is not loopback -- a typo of `0.0.0.0:7600` would
publish a write endpoint to the local network, so it fails at startup
rather than at first request. The loopback check parses the host and
accepts `localhost` by name alongside anything `net.IP.IsLoopback`
accepts, since `net.ParseIP("localhost")` returns nil.

`cmd/trackkrd/main.go` starts the listener alongside the reporter
goroutine and shuts it down with the same context, so `run` still returns
only when everything has stopped.

### Step 3: `manifest.json` and `background.js`

Permissions: `tabs`, `storage`, `idle`, and -- crucially -- the daemon
origin as `optional_host_permissions` covering `http://127.0.0.1/*` and
`http://localhost/*`, not a hardcoded `http://127.0.0.1:7600/*`.

Two problems collapse into one solution here. `extension_addr` is
configurable, so a pinned port in the manifest breaks silently the moment
someone changes it: the fetch loses its host permission, CORS applies, the
daemon answers no preflight, and every request fails with no visible
cause. And Firefox treats MV3 host permissions as optional anyway, so they
have to be requested at runtime regardless. So the options page takes the
daemon URL, calls `permissions.request({origins: [url]})` for exactly that
origin, and stores both. One mechanism covers the configurable port and
the permission prompt, and the popup's fourth state reports when it is
missing.

The host permission is what lets the background script's fetch skip CORS
entirely; without it every request needs the daemon to answer preflights,
which it deliberately does not.

The background script keeps `{tabId, url, title, startedAt}` in
`storage.session` and, on `tabs.onActivated`, `tabs.onUpdated` (URL or
title change), `windows.onFocusChanged`, and `runtime.onSuspend`,
finalizes the previous tab and starts the next.

Rules that decide what is reported:

- incognito windows are skipped entirely, never stored, never sent;
- `about:`, `view-source:`, and extension pages are skipped;
- losing focus to another application (`WINDOW_ID_NONE`) finalizes the
  current tab, so switching to a terminal does not credit Firefox with
  the next hour;
- focus moving to a *different Firefox window* finalizes the current tab
  and starts the newly focused window's active tab, which is the case a
  `WINDOW_ID_NONE`-only rule silently gets wrong;
- **idle**: the `idle` permission plus `browser.idle.onStateChanged`
  finalizes the current tab when the state goes `idle` or `locked` and
  starts a fresh one on `active`, with `setDetectionInterval` matched to
  the daemon's `idle_threshold`. Without this the extension has no
  equivalent of the daemon's idle handling at all: a tab left focused
  over lunch reports the whole gap, the absurd-duration cap is far too
  coarse to catch ninety minutes, and extension rows inflate against
  desktop rows -- which would poison the very data Phase 6 needs to
  deduplicate them;
- tabs shorter than one second are dropped. This is a new rule, not
  parity with the daemon, whose tracker only discards durations of zero
  or less; sub-second flicks through a tab strip are noise worth losing.

Delivery failures go in `storage.local`, not `storage.session`, and retry
on the next event. Session storage evaporates when Firefox exits, and
"unreachable while the browser is quitting" is precisely when a queue
earns its keep -- the daemon persists its own queue to `pending.json` for
the same reason. Current-tab state stays in `storage.session`, where a
stale focus from a previous run *should* disappear.

`runtime.onSuspend` enqueues first and then attempts delivery: a fetch
started during suspension is not guaranteed to finish, so the record has
to be durable before the attempt, not after it.

### Step 4: `popup.html` and options

The popup shows one of four states, and the fourth is the one that makes
a fresh install diagnosable:

1. connected -- `/extension/status` returned ok;
2. daemon unreachable -- the fetch failed with the permission granted;
3. token rejected -- 401, so the daemon is up and the token is wrong;
4. permission not granted -- `permissions.contains()` was false, with a
   button calling `permissions.request()`. That call needs a user
   gesture, which is why it lives in the popup or options page and cannot
   be done from the background script.

Without state 4, a declined or revoked host permission presents as state
2 and sends the user hunting a daemon that is running fine.

The options page stores the token in `storage.local`, next to the retry
queue. Both pages ship their own CSS; the extension does not share the
dashboard's stylesheet.

### Step 5: end-to-end

With `trackkrd` running and the extension temporarily installed, browse
a few tabs and confirm the records reach the dashboard with URLs attached.

## Known Overlap

While Firefox has focus, the desktop tracker also records it, so that time
is counted twice: once as `firefox` (lowercase, from WM_CLASS) and once as
`Firefox` with a URL. The casing split is deliberate rather than sloppy --
the two rows hash to different colours and group separately, which is what
makes the double-count visible instead of quietly doubling one bar.

Because the extension has its own idle rule (step 3), the two sides now go
idle on the same threshold, so the overlap is a consistent factor of two on
focused browsing rather than an ever-widening gap. That is what makes the
data usable for the deduplication parked in Phase 6.

## Verification

1. `mise test` and `mise test-race` pass; coverage stays at or above 50%.
2. `mise lint` clean.
3. `curl` against the listener: no token is 401, wrong token is 401, a web
   `Origin` is 403, `text/plain` is 415, a valid record is 202.
4. `ss -ltn` (or `lsof -i`) shows the listener bound to `127.0.0.1`, not
   `0.0.0.0`.
5. Load the extension with `about:debugging`, browse, and confirm rows
   with URLs appear on the dashboard for the daemon's device.
6. Open a private window, browse, and confirm nothing from it is reported.
7. Revoke the host permission in `about:addons` and confirm the popup
   says so rather than reporting the daemon as unreachable.
8. Leave a tab focused past the idle threshold and confirm the reported
   duration stops at the threshold instead of covering the whole absence.
