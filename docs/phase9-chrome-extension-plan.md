# Phase 9: Google Chrome Extension

## Goal

Add a Google Chrome Manifest V3 extension that reports active tabs to the
existing `trackkrd` loopback listener. Reuse the Firefox extension's tracking,
privacy filtering, durable queue, settings, and popup wherever the browser APIs
permit it.

The first release supports Google Chrome only. Chromium, Brave, Edge, Chrome
Beta, Chrome Canary, Chrome Web Store submission, and automated installation of
a Chrome binary are out of scope. The deliverables are an unpacked extension
directory and a versioned zip.

The daemon must be upgraded before the Chrome package is used. An old daemon
would otherwise attribute every browser record to Firefox, so compatibility
must fail visibly rather than silently corrupting application totals.

## User-visible behavior

- Chrome records appear under `Google Chrome` in application totals.
- Overlapping desktop Chrome observations are removed from the dashboard in
  the same way overlapping desktop Firefox observations are removed today.
- Firefox and Chrome can run simultaneously on one device without one
  browser's records suppressing the other browser's records.
- Website totals continue to derive from the URL-bearing browser records with
  no browser-specific presentation.
- Incognito tabs are never stored or sent.
- Ignore rules, idle handling, focus handling, offline queueing, and the
  one-second minimum segment duration remain identical across Firefox and
  Chrome.
- The Chrome popup distinguishes connected, token rejected, host permission
  missing, foreground local-network connection blocked, and daemon upgrade
  required. A network failure after host permission is granted must not pretend
  it can distinguish an LNA denial from a stopped daemon.

## Daemon contract

Keep the existing endpoint as the permanent Firefox-compatible route:

```text
POST /extension/activity
```

Add a Chrome-specific route with the same request and response bodies:

```text
POST /extension/activity/chrome
```

The route, rather than a caller-controlled `app_name` or an optional JSON field,
selects the canonical application name. The existing route creates `Firefox`
records; the Chrome route creates `Google Chrome` records. This design prevents
an old daemon from accepting a Chrome-only field it does not understand and
silently storing the batch as Firefox. Unknown browser routes return 404 and
the extension retains its queue.

Extend the authenticated status response to advertise supported producers:

```json
{"ok":true,"browsers":["firefox","chrome"]}
```

Existing Firefox builds ignore the added field. The Chrome popup requires
`chrome` in the list and otherwise displays `Daemon upgrade required`. The
background delivery path posts directly to the Chrome endpoint; a non-2xx
response leaves every record queued.

Both activity routes retain the current limits and validation:

- bearer token authenticated with constant-time comparison;
- `Content-Type: application/json` required;
- one bounded JSON value with at least one record;
- only HTTP and HTTPS page URLs;
- timestamps required, sub-second segments dropped, and segments over 12 hours
  clamped;
- batch accepted with HTTP 202 after valid records are enqueued.

Replace the current origin prefix check with parsed origin validation. An absent
`Origin` header is allowed. Otherwise, require exactly one header field-value
and parse it as a URL. Require an exact `moz-extension` or `chrome-extension`
scheme, an empty opaque component, a non-empty `Hostname()`, an empty `Port()`,
`Host == Hostname()`, no user information, and empty path, query, and fragment
components. Reject duplicate or comma-joined values, `null`, malformed values,
empty extension IDs, port-bearing origins, and every web origin. This validates
the request origin; it is separate from the loopback host-permission patterns
produced by `originFor`. The bearer token and JSON requirement remain mandatory
even for an accepted extension origin.

## Record identity and replay

Replace `UNIQUE (device_id, started_at)` with an explicit stable record identity.
Add migration `006_activity_record_identity` with a non-null UUID `record_id`
and a non-null `producer` column constrained to `desktop`, `firefox`, or
`chrome`. Backfill existing rows with generated record IDs and classify existing
URL-bearing `Firefox` rows as `firefox`; all other existing rows remain
`desktop`. Drop the old two-column unique constraint only after the backfill,
add `UNIQUE (device_id, record_id)`, and change `InsertActivityRecords` to
conflict on the new identity.

The daemon stamps the producer rather than trusting an extension-supplied
application name:

- native tracker records use `desktop`;
- the legacy loopback route uses `firefox`;
- the Chrome loopback route uses `chrome`.

Assign one record ID when a logical segment is created and preserve it through
every retry. The native tracker generates a UUID from `crypto/rand` when it
finalizes a new record. Browser tracking generates `crypto.randomUUID()` when a
segment starts and stores the ID in `storage.session.current`, then carries it
into the durable queue. IDs use the canonical lowercase 36-character UUID text
form. The daemon validates and preserves browser-provided IDs rather than
regenerating them; no third-party UUID package is required.

Carry both fields through `tracker.Record`, `pending.json`, the authenticated
ingest request, and `ActivityRecordRow`. Before the database migration ships,
release daemon compatibility code that assigns stable IDs to legacy pending and
Firefox extension records that lack one; derive those compatibility IDs from a
SHA-256 digest of the normalized producer and complete record content so the
same legacy replay receives the same version-8 UUID. The existing backend
ignores the new JSON fields, allowing this daemon-first rollout. Once migration
006 is deployed, the authenticated activity API requires a canonical record ID,
rejects unknown non-empty producer values, and normalizes only a missing legacy
producer (`firefox` for a URL-bearing Firefox record, otherwise `desktop`). This
is an intentional API compatibility change: upgrade `trackkrd` and any direct
ingest clients before applying migration 006.

A reporter retry preserves the record ID and conflicts as a replay. Distinct
Firefox, Chrome, desktop, tab, or window records survive even when they share a
device and start timestamp. The response's accepted count continues to count
only inserted rows.

## Browser identity and deduplication

Generalize the current Firefox-only deduplicator around an internal browser
family. The initial aliases are:

| Family | Canonical extension name | Desktop application keys |
| --- | --- | --- |
| Firefox | `Firefox` | `firefox` |
| Chrome | `Google Chrome` | `google chrome`, `google-chrome` |

Key browser coverage by both device ID and the trusted producer column:
`firefox` or `chrome`. Firefox coverage may subtract only `desktop` records in
the Firefox alias family on the same device; Chrome coverage may subtract only
`desktop` records in the Chrome alias family on the same device. URL-bearing
`desktop` records with an unknown application family continue to render
normally and do not subtract desktop observations. Do not infer an extension
producer solely from a caller-controlled application name once the producer
column exists.

After subtraction, canonicalize every surviving record in a known browser
family before aggregation. URL-bearing Firefox activity and residual desktop
records named `Firefox` or `firefox` contribute to one `Firefox` total.
URL-bearing Chrome activity and residual desktop records named `Google Chrome`,
`google chrome`, or `google-chrome` contribute to one `Google Chrome` total.
Unknown application names retain their stored display names.

Resolve the `Google Chrome` icon through an explicit ordered alias list:
`google chrome`, then `google-chrome`. Fetch both rows in one query, index them
by normalized key, and select the first present candidate; never rely on
database result order. This preserves the macOS `Google Chrome` icon and the
Linux X11 `google-chrome` icon without renaming stored activity or icon rows.

## Shared extension runtime

Keep `extension/manifest.json` as the Firefox manifest. Add a Chrome source
manifest, `extension/manifest.chrome.json`, with:

- Manifest V3;
- `background.service_worker` pointing to `background-cr.js`;
- `minimum_chrome_version: "116"`; [`storage.session` exists from Chrome
  102](https://developer.chrome.com/docs/extensions/reference/api/storage), but
  the Promise form of [`idle.queryState()` requires Chrome
  116](https://developer.chrome.com/docs/extensions/reference/api/idle), which
  is the latest gating API awaited by this runtime; do not add a
  callback-to-Promise polyfill;
- `tabs`, `storage`, and `idle` permissions;
- `optional_host_permissions` for `http://127.0.0.1/*`,
  `http://localhost/*`, and `http://[::1]/*`;
- the existing action popup, options page, and icons;
- no Gecko-only keys.

Split background startup from the shared tracking implementation. Move the
browser-neutral state machine, queue, delivery, and listener callbacks from
`background.js` into `background-core.js`. Expose a synchronous listener
registration function and a separate asynchronous recovery function. Keep
lifecycle registration in thin browser entrypoints:

- `background-fx.js` registers the core listeners, owns the Firefox-only
  `runtime.onSuspend` registration, and then starts asynchronous recovery;
- `background-cr.js` synchronously calls `importScripts` for `logic.js`,
  `common.js`, and `background-core.js`, registers the core listeners without a
  suspension hook, and only then starts asynchronous recovery.

Following Chrome's [synchronous listener registration
requirement](https://developer.chrome.com/docs/extensions/develop/migrate/to-service-workers#register-listeners),
registration must finish during global script evaluation and before the first
storage read, Promise continuation, timer, or other asynchronous operation. This
includes every `tabs`, `windows`, `idle`, `storage`, `runtime.onStartup`, and
`runtime.onInstalled` listener. Listener callbacks may start serialized
asynchronous work after registration. Recovery and best-effort delivery are a
second phase and cannot gate listener installation, so the event that wakes a
new Chrome worker is never missed.

Only `background-fx.js` may access `runtime.onSuspend`. The Chrome runtime does
not provide that event, and `background-cr.js` must initialize successfully when
the property is absent. Chrome service-worker termination must not itself
finalize or clear the current segment.

Segment finalization uses an idempotent, queue-first handoff. Read and validate
the current segment, build the record, and re-check the ignore list. For an
accepted record, append it to the durable `storage.local` queue unless that
record ID is already present, await the queue write, and only then
remove the current segment from `storage.session`. If queue persistence fails,
leave the current segment intact and propagate the failure. Invalid, incognito,
or newly ignored segments may be cleared without queueing. Termination after the
queue write but before the session clear leaves both copies; recovery recognizes
the matching queued identity and clears the session copy without appending it
again. The database record identity provides the final replay guard.

Treat persisted session state as untrusted input during recovery. A current
segment is valid only when it has a valid record UUID, positive integer tab and
window IDs, a finite start timestamp no later than the recovery time, an HTTP or
HTTPS URL, and an explicit `incognito === false`, and it does not match the
current ignore list. Store that explicit incognito value when starting every new
segment. Discard invalid, future, incognito, and newly ignored state without
queueing. If valid state still
matches the focused active tab and URL while the user is active, keep its
original start time. Otherwise, finalize it at the recovery event time through
the queue-first handoff before starting any newly focused tab. A missing or
closed old tab is therefore finalized, while corrupt state is never converted
into activity.

Handle state written by the previous Firefox build as an explicit one-time
upgrade case. If `record_id` or `incognito` is absent, query the referenced live
tab first. Only when that tab still exists, is non-incognito, and matches the
stored window and URL may recovery generate a record ID, set
`incognito: false`, and await rewriting `storage.session.current` before any
other transition. If the live tab cannot prove those facts, discard the legacy
current state without queueing; privacy takes precedence over recovering one
unverifiable segment.

Update the Firefox manifest's ordered classic scripts to load `logic.js`,
`common.js`, `background-core.js`, and `background-fx.js`. Do not convert the
shared files to modules: Firefox still loads ordered classic background scripts,
and the Node harness relies on the same order.

`common.js` continues to expose `api = browser ?? chrome` and also exposes a
browser kind derived from the available namespace. This shared compatibility
layer handles API naming and endpoint selection; lifecycle differences remain
in the two background entrypoints. Firefox selects the legacy activity endpoint;
Chrome selects `/extension/activity/chrome`. No user-agent or brand detection is
added because non-Google Chromium browsers are outside this phase.

Make extension UI copy browser-neutral except for the explicit daemon upgrade
message. Chrome and Firefox keep separate browser-managed settings and queues,
but use the same daemon URL and configured bearer token.

Chrome 142 and newer independently gate loopback requests with [Local Network
Access](https://developer.chrome.com/blog/local-network-access) (LNA), in
addition to the extension host permission. After a user gesture successfully
grants the optional host permission, the Chrome options page or popup must
immediately issue the authenticated status `fetch` from that foreground
extension document. The foreground request, not the service worker, triggers
Chrome's LNA prompt. Do not ask the background worker to trigger it;
service-worker requests remain blocked until the extension origin has already
been granted LNA.

Track extension host permission separately from foreground network readiness. A
failed `permissions.contains` check is `Host permission missing`. Once host
permission exists, a foreground request that fails before receiving an HTTP
response is `Local network access blocked or daemon unavailable`; JavaScript
cannot reliably distinguish those two network failures, so the UI must show
both the LNA recovery path in Chrome's site settings and the daemon-running
check, then offer a foreground Retry action. Only after this foreground status
request succeeds may background delivery be described as connected. A 401 is
`Token rejected`. Successful Chrome status parsing requires a 2xx response whose
JSON body has `ok === true` and a `browsers` array containing the exact lowercase
string `chrome`. Missing, malformed, or differently cased capability data is
`Daemon upgrade required`, not `Connected`.

## Build and development workflow

Add `mise run ext-build-chrome`. It must:

1. Stage only runtime files in `dist/chrome/`.
2. Copy `manifest.chrome.json` to `dist/chrome/manifest.json`.
3. Exclude Firefox metadata, tests, source manifests, and development files.
4. Produce `dist/trackkr-chrome-<manifest-version>.zip`.
5. Fail when a referenced runtime file is missing or the zip cannot be
   validated.

`dist/` remains ignored. The build uses the repository's pinned Node toolchain
and ordinary archive tooling; it does not download or launch Chrome.

Keep `extension/manifest.chrome.json` as the Chrome source manifest, but exclude
it and `background-cr.js` from the Firefox `web-ext lint` input. Update
`mise run ext-lint` so its `--ignore-files` patterns cover `tests/**`,
`manifest.chrome.json`, and `background-cr.js`; it continues to validate the
root `extension/manifest.json` as Firefox source. `mise run ext-run` likewise
continues to launch that Firefox manifest and is not a Chrome validator.
Use the explicit lint command:

```sh
web-ext lint --source-dir extension --warnings-as-errors \
  --ignore-files 'tests/**' 'manifest.chrome.json' 'background-cr.js'
```

Add a separate Chrome package validation task that operates only on the clean
`dist/chrome/` staging directory and run it in the extension CI job after the
shared Node tests. The build must delete and recreate `dist/chrome/`, then copy
runtime files from an explicit allowlist before creating the zip, so stale files
cannot leak into a later package. Update the development output and
`extension/README.md` with these Chrome steps:

1. Run `mise run ext-build-chrome`.
2. Open `chrome://extensions` and enable Developer mode.
3. Choose Load unpacked and select `dist/chrome/`.
4. Paste the existing daemon token in Settings and grant loopback access.

Update `docs/plan.md` so the browser extension and deduplication sections name
both Firefox and Google Chrome and point to this phase plan.

## Automated tests

### Go

- Existing Firefox route still stamps records as `Firefox`.
- Chrome route stamps records as `Google Chrome`.
- Unknown browser paths cannot enqueue records.
- Status advertises both supported browsers.
- Absent origins and exact Chrome and Firefox extension origins are accepted
  with a valid token.
- `null`, empty, malformed, credentialed, path-bearing, query-bearing,
  fragment-bearing, port-bearing, duplicate, comma-joined, prefix-confusable,
  and web origins are rejected.
- Existing method, token, content type, body, timestamp, URL, duration, and
  shutdown tests pass for the refactored handler.
- Migration 006 backfills existing Firefox extension rows and record IDs,
  preserves existing rows, and enforces the producer enum and new unique
  identity.
- Same-device, same-start desktop, Firefox, Chrome, and same-browser window
  records with distinct IDs all insert; replaying any ID inserts no additional
  row.
- Legacy records without IDs receive stable compatibility IDs before cutover.
  Missing legacy producer values normalize deterministically, malformed IDs and
  unknown non-empty producers are rejected.
- Chrome desktop and extension overlaps deduplicate on macOS and Linux aliases.
- Firefox and Chrome coverage on the same device remains family-scoped.
- Coverage on one device never subtracts another device.
- URL-bearing and residual desktop records for `Firefox` and `firefox` aggregate
  into one `Firefox` total. The three specified Chrome aliases aggregate into
  one `Google Chrome` total.
- App totals, timeline truncation, and work limits retain their current bounds.
- Chrome icon resolution covers exact-only, fallback-only, both aliases in both
  database result orders, and neither alias.

### Extension

- Load `background-core.js` with `background-fx.js` under a Firefox `browser`
  namespace and with `background-cr.js` under a Chrome-only `chrome` namespace.
- The Firefox harness exposes `runtime.onSuspend` and verifies that only the
  Firefox entrypoint registers it.
- The Chrome harness omits `runtime.onSuspend` entirely and verifies that worker
  startup, event registration, tracking, and delivery complete without a
  `TypeError`.
- Immediately after script evaluation and before advancing a microtask, the
  Chrome harness observes every required listener. Storage reads, recovery, and
  delivery have not yet begun.
- Refactor the harness into durable fake browser state plus disposable worker
  instances. Destroying a worker discards its VM, globals, pending callbacks,
  and listener registry while retaining `storage.session`, `storage.local`,
  tabs, windows, focus, and idle state. A fresh worker reloads scripts through an
  `importScripts` shim and receives subsequent events only through its new
  listeners.
- Worker restart tests cover termination between settled events, including a
  wake event for a different tab and recovery of closed, ignored, incognito,
  future-dated, and malformed current state.
- A deterministic storage fault makes the session removal fail after the
  durable queue write succeeds. Verify finalization returns with both copies
  intact, then discard the settled worker and create a fresh one. Recovery must
  recognize that the current session entry has the same ID, clear it without a
  second append, and later deliver idempotently. This unit test proves the write
  ordering without claiming the harness can force-kill real in-flight Chrome
  work; that scenario remains a manual Chrome test.
- Chrome delivery uses `/extension/activity/chrome`; Firefox keeps
  `/extension/activity`.
- A rejected or unavailable Chrome endpoint leaves the queue intact.
- Chrome options request host permission before the user gesture expires, then
  issue the status request from the foreground document rather than the worker.
- Chrome popup and options tests distinguish missing host permission from a
  foreground loopback request blocked before connection, and cover the LNA
  recovery copy and Retry action. The actual Chrome LNA prompt is manual-only.
- Chrome status tests require 2xx, `ok === true`, and exact lowercase `chrome`;
  malformed JSON, missing arrays, and casing differences report an upgrade.
- Both manifests declare every loopback host-permission pattern returned by
  `originFor`, including `127.0.0.1`, `localhost`, and bracketed IPv6. These are
  manifest permissions, not request `Origin` values.
- The Chrome manifest uses a service worker, has no Gecko keys, and declares the
  supported minimum version of 116 and the exact required and optional
  permission keys.
- Firefox lint excludes the Chrome source manifest and Chrome-only entrypoint;
  Chrome package validation runs against the clean staged allowlist.
- The staged unpacked directory contains exactly the required files and the zip
  passes an archive integrity check.

## Manual acceptance

With the upgraded daemon running, load the unpacked Chrome build in Chrome 142
or newer and confirm:

1. Granting the optional host permission is followed by a foreground status
   request that triggers the LNA prompt. Granting LNA connects; denying it shows
   the distinct recovery instructions, and Retry works after enabling access in
   Chrome site settings.
2. Tab activation, navigation, title changes, multi-window focus, browser focus
   loss, tab closure, idle, lock, and resume create the expected segments.
3. Incognito and ignored sites never appear in session storage, the durable
   queue, daemon logs, or the dashboard.
4. Records survive daemon downtime and send after it returns.
5. Force-stop the extension service worker during finalization. Restarting does
   not lose or truncate the segment, and the record identity prevents a
   replay from creating a second database row.
6. The dashboard shows `Google Chrome`, website totals, and the Chrome icon
   without overlapping desktop Chrome time.
7. Firefox and Chrome can run together without cross-browser subtraction.
8. A Chrome build pointed at an old daemon reports the upgrade requirement and
   does not misattribute or discard its queued records.
9. A wrong token is reported separately from host permission, LNA, connection,
   and daemon capability failures.

## Implementation sequence

1. Commit the unrelated dashboard load-more work before starting this phase.
2. Add daemon-side record ID generation and legacy normalization while the old
   backend still ignores those fields.
3. Add migration 006, record identity and producer propagation, replay tests,
   daemon routing, parsed origin validation, and status capabilities.
4. Generalize browser-family totals, deduplication, and ordered Chrome icon
   aliases.
5. Refactor the shared extension runtime, queue-first finalization, restartable
   harness, and synchronous listener registration.
6. Add the Chrome manifest, service worker, LNA foreground flow, staging script,
   zip task, and package validation.
7. Update CI, the extension README, development output, and `docs/plan.md`.
8. Run `mise run format`, `mise run test`, `mise run test-race`,
   `mise run test-coverage`, `mise run lint`, `mise run hooks`,
   `mise run ext-test`, `mise run ext-lint`, and the Chrome package checks.

Keep the daemon/deduplication work, shared extension refactor, and Chrome
packaging/documentation as separate semantic commits.
