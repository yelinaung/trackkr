# Phase 6: macOS Application Icons

## Decision

Phase 6 ships application icons for macOS activity only.

The first icon slice deliberately excludes Firefox favicons and Linux
application icons. macOS already provides the frontmost process PID, AppKit
can return the installed application icon without Accessibility or Screen
Recording permission, and this is the platform available for native manual
verification. Shipping this slice first proves the storage, upload, caching,
and dashboard contracts with one reliable producer.

Icons are disposable presentation metadata. The daemon keeps pending icons in
memory and sends them through the reporter's existing flush loop, but does not
create a second durable queue. A crash may delay an icon until the application
is observed again; icon work must never weaken activity persistence or block
activity capture.

The resulting data flow is intentionally small:

```text
CoreGraphics PID -> NSRunningApplication.icon -> trackkrd memory queue
                                                  |
                                                  v
                                         POST /api/v1/app-icons
                                                  |
                                                  v
                                              app_icons
                                                  |
                                                  v
                                           dashboard app rows
```

## Scope

This phase delivers:

- application icons beside macOS app-total rows;
- a user-scoped PostgreSQL table for validated PNG icons;
- an authenticated device upload endpoint with bounded write rate;
- an authenticated, cacheable image endpoint for the dashboard;
- best-effort, deduplicated delivery through the existing reporter loop;
- a deterministic fallback when no icon is available.

Timeline bars remain coloured SVG rectangles. They are often narrower than an
icon, and the existing app colour is the useful visual link between a total
and its bars.

## Non-Goals

- Firefox or other browser favicons.
- Any change to the Firefox extension.
- Linux application icons or freedesktop desktop-entry resolution.
- A durable `pending-icons.json` queue.
- A second reporter mutex, goroutine, ticker, flush interval, or environment
  variable.
- Changing application aggregation, browser/desktop deduplication, or
  historical activity rows.
- The trailing-dot `siteExpr` correction; that is an independent, low-cost
  correctness fix rather than a prerequisite for application icons.
- An editable icon catalogue, icon picker, or manual upload UI.
- Serving SVG or fetching an icon from the network.

## Identity

The dashboard currently groups app totals by `activity_records.app_name`.
Application icons follow that identity without adding bundle identifiers to
activity records:

```go
func AppKey(name string) string {
    return strings.ToLower(strings.Join(strings.Fields(name), " "))
}
```

An app key must be non-empty after normalization and no longer than 255 UTF-8
bytes. A name that cannot produce a valid key produces no icon and does not
affect activity tracking.

This choice has two explicit consequences:

- `Firefox` and `firefox` share one icon even though they can remain separate
  totals until activity deduplication is implemented;
- two applications with the same normalized display name share one icon, and
  the last accepted upload wins.

The second rule also applies across devices. A Mac and a future Linux client
can normalize their local Firefox names to the same key while producing
different artwork. Startup or the 30-day refresh can change which icon is
shown. That is a known last-writer-wins limitation of a user-wide display-name
key, not digest ping-pong within either client. Device-specific icons or a
stable application identifier would require changing the dashboard's identity
model and are outside this slice.

The macOS bundle identifier remains useful locally for diagnostics, but is not
stored or used as the server key in this phase.

## Image Contract

`internal/icon` owns the shared type, app-key normalization, digest, and PNG
validation used by the tracker and server:

```go
package icon

type App struct {
    Key string `json:"key"`
    PNG []byte `json:"png"`
}
```

Validation errors wrap an exported `ErrInvalid` sentinel and include
field-specific context. The exact contract is:

- `Key` equals `AppKey(Key)` and is between 1 and 255 bytes;
- PNG signature is present;
- encoded PNG is at most 64 KiB;
- `png.DecodeConfig` succeeds before allocation;
- width and height are each between 1 and 128 pixels;
- a complete `png.Decode` succeeds after the dimension check;
- SHA-256 is computed from PNG bytes by trackkr and is never accepted from
  request JSON.

The macOS producer renders onto a transparent 64 by 64 canvas, preserving the
source aspect ratio and centring any padding. The 128-pixel validation ceiling
allows a high-density producer without permitting unbounded decode work.

The package performs no HTTP, database, filesystem, or platform-native work.
It is justified as a package because the tracker and server must share one
security boundary. This phase adds no Go dependency.

## Database

Migration `003_app_icons` adds one application-specific table:

```sql
CREATE TABLE app_icons (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    app_key      TEXT NOT NULL CHECK (
                     app_key <> '' AND octet_length(app_key) <= 255),
    png          BYTEA NOT NULL CHECK (octet_length(png) <= 65536),
    sha256       BYTEA NOT NULL CHECK (octet_length(sha256) = 32),
    width        SMALLINT NOT NULL CHECK (width BETWEEN 1 AND 128),
    height       SMALLINT NOT NULL CHECK (height BETWEEN 1 AND 128),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, app_key)
);
```

`BYTEA` keeps deployment and backup behaviour consistent with the rest of the
database. `AppIconUserLimit` caps each user at 512 rows, or 32 MiB at the
per-icon maximum. Old rows beyond the cap are removed in ascending
`last_seen_at, id` order.

The upsert and prune run in one transaction. Before either statement, the
transaction executes:

```sql
SELECT pg_advisory_xact_lock($1::bigint);
```

This application-icon-specific per-user lock is deliberate. Under PostgreSQL's
default isolation, two devices can otherwise each prune against a snapshot
that does not include the other's inserts and commit above the cap. The
positive user ID key serializes application-icon retention for that account;
site icons use the negative key so the two subsystems remain independent.

After acquiring the user lock, the transaction reads one `clock_timestamp()`
and passes it explicitly to every insert and update in the batch. PostgreSQL's
`NOW()` is fixed at transaction start, so using it after a lock wait could make
a newly accepted icon older than the upload that just released the lock and
allow the new row to prune itself. One post-lock timestamp also prevents a
changed icon's `updated_at` from moving backward.

An identical digest updates only `last_seen_at`. A changed digest also updates
the PNG, dimensions, digest, and `updated_at`. The query returns the number of
evicted rows; the API handler logs a warning with user ID and count, but never
the app keys.

The database layer adds:

```go
func (q *Queries) UpsertAppIcons(
    ctx context.Context,
    userID int64,
    icons []icon.App,
) (evicted int, err error)

func (q *Queries) AppIconMetadata(
    ctx context.Context,
    userID int64,
    keys []string,
) ([]AppIconRow, error)

func (q *Queries) AppIcon(
    ctx context.Context,
    userID int64,
    id int64,
) (*AppIconRow, error)
```

`AppIconRow` lives in `internal/db/models.go` with the existing row types.
Metadata queries do not select PNG bytes. The down migration drops
`app_icons`; no activity migration or backfill is required.

The server defines interfaces at the two consuming boundaries rather than
widening the existing activity and web query interfaces:

```go
type appIconWriter interface {
    UpsertAppIcons(context.Context, int64, []icon.App) (int, error)
}

type appIconReader interface {
    AppIconMetadata(context.Context, int64, []string) ([]db.AppIconRow, error)
    AppIcon(context.Context, int64, int64) (*db.AppIconRow, error)
}
```

Production assigns the same `*db.Queries` to both. Tests can fake the upload
or dashboard boundary without implementing unrelated activity methods.

## Upload API

The API-key-authenticated device routes add:

```text
POST /api/v1/app-icons
X-API-Key: existing device key
Content-Type: application/json
```

The JSON request is a small batch. Go represents `[]byte` as base64:

```json
{
  "icons": [
    {"key":"safari","png":"iVBORw0KGgo..."}
  ]
}
```

The handler applies limits before database work:

- method `POST` and JSON content type;
- body capped at 1 MiB with `http.MaxBytesReader`;
- exactly one JSON value with no trailing data;
- 1 through 10 icons per batch;
- no duplicate key within a batch;
- every icon passes the shared validation contract;
- ownership comes only from the authenticated device's `UserID`.

Duplicate keys return `400` instead of defining ordering. This also avoids a
multi-row PostgreSQL upsert trying to affect the same conflict row twice.
Valid batches return `200` with `{"accepted": n}`.

### Write Rate

The route has a process-local, per-device sliding-window limiter. It reserves
one request before reading or decoding the body, so invalid requests consume
capacity too:

- 60 requests per device per hour;
- at most 10 icons per request, bounding accepted work to 600 icon rows per
  device per hour;
- check and reservation are one mutex-protected operation;
- rejected requests return `429` and `Retry-After` as the ceiling in seconds
  until the oldest retained hit expires;
- at most once per hour, a sweep removes expired hits and empty device buckets
  so the limiter map stays bounded.

The limiter is abuse protection, not a durable quota; a server restart resets
it. The authenticated per-user database lock and 512-row cap remain the
storage invariant. A dedicated limiter in `app_icons.go` avoids refactoring
the login-attempt limiter as part of this feature.

The reporter retries network errors, `401`, `403`, `404`, `429`, and `5xx`
with exponential backoff from one through 15 minutes. A valid `Retry-After`
value can extend that delay. For `400`, `413`, or `422`, a multi-icon batch is
retried as individual icons so accepted neighbours are removed only after the
server owns them and only a permanently rejected snapshot is dropped.

## Dashboard Delivery

The session-authenticated web routes add:

```text
GET /app-icons/{id}/{sha256}.png
```

The URL contains an opaque row ID and lowercase digest, not an application
name. The query includes both `user_id` and `id`; another user's row and a
missing row both return `404`. The digest in the path must match the current
row, so changed bytes receive a new immutable URL.

Successful responses set:

```text
Content-Type: image/png
Cache-Control: private, max-age=31536000, immutable
ETag: "<sha256>"
Vary: Cookie
```

`Vary: Cookie` partitions browser-profile cache entries by the authenticated
session cookie, preventing an account switch from reusing another user's
fresh immutable response for the same URL. `If-None-Match` returns `304`, and
both the `200` and `304` response carry the isolation header. The existing
`nosniff` header and
`img-src 'self' data:` policy already permit this route without expanding the
CSP.

The dashboard loads app totals as today, derives their app keys, performs one
metadata query, and joins rows in Go. Once an icon is learned, it decorates old
activity totals on their next render; no backfill is needed.

Each app total keeps its colour swatch and adds a fixed 22-pixel icon. A
missing icon renders a rounded monogram tinted with the existing app colour.
The monogram uses the first two `unicode.IsLetter` or `unicode.IsDigit` runes,
uppercased, or `?` when neither exists. Its 10-pixel text is black or white
according to the chip's computed relative luminance, keeping every generated
hue at a contrast ratio of at least 4.5:1. Images use `alt=""` because adjacent
text already names the app. Site totals do not change in this phase.

## Best-Effort Daemon Delivery

The reporter adds an in-memory map keyed by app key:

```go
func (r *Reporter) EnqueueAppIcon(appIcon icon.App) bool
```

The Darwin adapter has already applied the shared validator. The reporter
copies the validated value before acquiring its existing mutex; the critical
section only updates the map. The newest digest replaces an older value for
the same key. At most 128 app keys wait; a new key beyond the cap returns
`false`, and the tracker retries on its next observation. A successful enqueue
sends a non-blocking signal on the existing `flushCh`; it does not create
another channel. The server independently validates the uploaded bytes at the
trust boundary.

No `pending-icons.json`, `iconMu`, second loop, second ticker, or icon-specific
configuration is added. Application icons are locally reproducible and do not
justify a second durability protocol.

The existing reporter loop calls two independent operations on every tick or
flush signal:

1. flush activity records first;
2. flush the app-icon map second.

One failure does not skip the other. Both use the existing mutex only to copy
or update their queues; neither holds it during network I/O. Each app-icon HTTP
request gets a five-second base timeout plus transfer time at 64 KiB per
second, capped at 25 seconds. The whole flush is also capped at 25 seconds, so
batch isolation cannot extend the loop indefinitely. Activity enqueueing
continues during that request because the reporter mutex is not held.

`flushAppIcons` sorts keys lexically, snapshots at most 10 entries, and leaves
them live during the request. On failure it removes nothing. On success it
removes a key only when the current digest still equals the uploaded digest. A
newly queued different digest therefore survives; a just-queued identical
digest is removed because the server already accepted those bytes. The
implementation comment records that intentional same-digest case.

A permanent `400`, `413`, or `422` response for multiple icons triggers
single-icon probes. Successful probes use the same digest-conditional removal;
permanently rejected probes drop only their matching poison snapshot. A newer
replacement remains eligible in either case.

Retryable failures retain every unresolved icon and schedule exponential
backoff from one minute through 15 minutes. `429` additionally honours either
the delta-seconds or HTTP-date form of `Retry-After`, capped at 24 hours. A
successful flush clears the failure count.

Shutdown keeps activity ahead of presentation metadata. It first performs the
existing final activity flush and persists any remaining activity records to
`pending.json`. It then attempts one best-effort app-icon flush even when the
pending write failed and even when a retry delay is active. The icon attempt
has a five-second shutdown budget. The shorter parent deadline overrides the
normal 25-second flush cap so disposable icon metadata cannot materially delay
termination after activity persistence has already failed. An icon error is
logged but does not replace the activity persistence result. The
next daemon start has an empty digest cache, so the next observation of each
app derives and queues its icon again. Icon work therefore cannot weaken
activity persistence.

This accepts a short-lived presentation gap in exchange for removing the
second persistence format, fsync protocol, recovery policy, reporter goroutine,
ticker, mutex, and shutdown path from the phase.

## macOS Acquisition

The existing CoreGraphics snapshot already returns the PID and display name of
the frontmost layer-zero window. The optional detector callback uses that PID
to query AppKit:

```objc
NSRunningApplication *app =
    [NSRunningApplication runningApplicationWithProcessIdentifier:pid];
NSImage *source = app.icon;
```

`NSRunningApplication.icon` reads application metadata and does not require
Accessibility or Screen Recording permission. It does not change the current
frontmost-window decision or introduce `NSWorkspace` as a detector.

The Objective-C boundary adds one function that returns a malloc-owned observed
application name, PNG buffer, and length, or no icon. Inside an autorelease pool
it renders the `NSImage` aspect-fit onto a transparent 64 by 64 bitmap and
encodes PNG. Every owned object and allocation is released on every return
path. The Go caller copies and frees both native buffers, normalizes the
observed name, and rejects the result unless it matches the application name
from the frontmost snapshot. This closes the PID-reuse race between the
CoreGraphics snapshot and the later AppKit lookup; the snapshot name remains
the server identity.

`window_darwin.go` links AppKit for this metadata operation and retains the
current Foundation, ApplicationServices, and CoreGraphics links. A build-time
size assertion verifies Darwin `pid_t` fits in Go `int` before the PID
round-trips through `appInfo.PID`.

### Cache

Native conversion is cached by `(PID, AppKey(appName))`:

- positive entries expire after five minutes;
- negative entries expire after 30 seconds;
- a reused PID with a different app key misses immediately;
- misses are marked in flight and offered non-blockingly to one bounded worker;
- polling returns without an icon immediately instead of waiting for AppKit;
- the clock and native callback are injected for platform-neutral tests;
- returned PNG slices are immutable.

The worker serializes AppKit conversions and bounds queued misses to 128. A
full queue drops only the new icon request; a later poll can offer it again.
Detector close stops accepting work and signals the worker without joining it,
because a cgo/AppKit call cannot be cancelled safely. A native conversion that
returns after close is discarded. Activity capture, cancellation, record
finalization, and pending-record persistence therefore never wait for icon
conversion.

The bounded positive expiry means an in-place app update with the same PID can
show its old icon for at most five minutes. That is acceptable for
presentation metadata. A same-PID, same-name process also shares the server
identity during that interval.

The detector logs native icon failure at debug level at most once per negative
cache lifetime. Missing or invalid icons never change `ActiveWindow`'s app
name, title, or error.

## Tracker Integration

`WindowInfo` gains one optional field:

```go
type WindowInfo struct {
    AppName string
    Title   string
    AppIcon *icon.App
}
```

`detectorCore` gains the optional callback
`iconForApp func(context.Context, appInfo) *icon.App` plus a non-blocking close
callback for its worker. The icon callback runs after
`frontmost` succeeds and before the title-permission branch. Turning
`macos_read_titles` off therefore disables Accessibility work without
disabling permission-free application icons. A nil callback preserves the
zero-value behaviour for Linux, no-cgo Darwin, unsupported platforms, and
existing fakes.

The tracker stores the last successfully enqueued digest and `lastQueuedAt`
for at most 128 app keys. It enqueues when a digest changes or an unchanged
icon has not been queued for 30 days. The refresh lets an active app recover
after database eviction. A full reporter queue does not advance this state, so
the next poll retries. Evicting the oldest tracker state merely causes another
best-effort announcement.

Activity start, transition, idle, and finalization behaviour remain unchanged.

## File Plan

New files:

```text
internal/icon/
  app.go                         # app key, PNG validation, digest
  app_test.go
internal/db/migrations/
  003_app_icons.up.sql
  003_app_icons.down.sql
internal/server/
  app_icons.go                   # upload, rate limiter, image handler
  app_icons_test.go
internal/tracker/
  app_icon.go                    # portable cache and tracker state helpers
  app_icon_test.go
```

Modified files:

```text
internal/db/models.go             # AppIconRow
internal/db/queries.go            # app-icon upsert, prune, lookup
internal/db/queries_test.go
internal/server/server.go         # narrow icon interfaces and routes
internal/server/testhelper_test.go
internal/server/handlers_web.go   # app-icon metadata join
internal/server/handlers_web_test.go
internal/server/templates.go      # IconURL and monogram fields
internal/server/templates_test.go
internal/tracker/window.go        # optional WindowInfo.AppIcon
internal/tracker/window_darwin.go # AppKit bridge wiring and PID assertion
internal/tracker/detector_core.go # optional iconForApp callback
internal/tracker/detector_core_test.go
internal/tracker/macos_darwin.h   # PNG buffer contract
internal/tracker/macos_darwin.m   # NSRunningApplication icon conversion
internal/tracker/reporter.go      # in-memory app-icon map and flush
internal/tracker/reporter_test.go
internal/tracker/tracker.go       # digest-aware app-icon enqueue
internal/tracker/tracker_test.go
web/templates/partials/timeline.html
web/static/style.css
deploy/README-macos.md
docs/plan.md
```

No extension, Linux detector, configuration, `go.mod`, or `go.sum` file
changes are part of this phase.

## Implementation Order

### Step 1: Shared contract and storage

Add `internal/icon`, migration 003, `AppIconRow`, database queries, the
per-user advisory lock, retention cap, and concurrent-upload test.

### Step 2: Server and dashboard

Add upload validation, the per-device write limiter, the session image route,
metadata lookup, and the app-row fallback. Exercise the full server path with
a fixture PNG before any native producer exists.

### Step 3: Best-effort reporter path

Add the bounded in-memory map and extend the existing reporter loop. Prove
that activity flushes run first, icon errors do not suppress activity, and
shutdown preserves the existing activity contract.

### Step 4: macOS producer

Add AppKit conversion, the bounded cache, detector callback, and tracker
digest state. Run portable tests first, then compile and manually exercise the
Objective-C boundary on macOS.

### Step 5: Documentation and verification

Update the overarching plan and macOS deployment notes, run the full test
matrix, and perform the manual multi-user and offline checks.

## Testing

### Shared Logic

Table-driven standard-library tests cover:

- empty, whitespace, case, repeated-space, Unicode, and 255-byte app keys;
- valid PNG, wrong signature, truncated PNG, invalid CRC, oversized bytes,
  zero dimensions, and dimensions above 128;
- complete decode after a successful configuration decode;
- stable SHA-256 digest and immutable copied bytes.

### Database

PostgreSQL tests cover:

- insert, identical replay, and changed-digest update;
- `updated_at` unchanged and `last_seen_at` refreshed for identical bytes;
- uniqueness scoped by user;
- pruning to 512 rows without crossing user boundaries;
- two concurrent upserts for one user never committing more than 512 rows;
- an upload delayed on the user lock receiving one post-lock timestamp and
  retaining its newly accepted row;
- concurrent uploads for different users not sharing the retention lock;
- metadata lookup omitting PNG and image lookup requiring the owning user;
- user deletion cascading to icons;
- byte, dimension, and digest constraints.

### HTTP

`httptest` and hand-written fakes cover:

- API-key authentication and ownership from device context;
- body, content type, batch count, duplicate key, trailing data, and PNG
  validation;
- one atomic limiter reservation under concurrent requests;
- invalid requests consuming rate capacity;
- `429` and accurate `Retry-After` after 60 requests;
- stale limiter buckets being swept;
- an eviction warning that contains no app key;
- session authentication, cross-user `404`, stale digest `404`, ETag, `304`,
  cookie-varying private immutable caching, and PNG content type.

### Reporter And Tracker

Tests cover:

- same-key replacement, 128-key capacity, and copied PNG ownership;
- activity upload attempted before app icons on the same flush;
- app-icon failure not suppressing activity success and vice versa;
- no mutex held while either network request blocks;
- body-scaled icon request timeout and 25-second flush cap with
  `testing/synctest` or explicit synchronization rather than real sleeps;
- success removing only a matching digest, including a just-queued identical
  digest, while preserving a changed digest;
- retryable response retention, exponential and `Retry-After` backoff, and
  permanent batch rejection isolating only the poison icon;
- shutdown attempting icons after the activity persistence attempt even when
  persistence fails or an icon retry delay is active;
- tracker changed-digest enqueue, 30-day refresh, 128-key state eviction, and
  no state advance after queue rejection;
- `mise test-race` across enqueue, flush, replacement, and shutdown.

### macOS

Platform-neutral cache tests inject the native callback and clock. They cover
positive and negative expiry, positive and negative PID reuse with a different
app key, an identity change during native lookup, invalid native PNG, immutable
slices, concurrent misses returning without waiting for a blocked native
loader, non-blocking close, and errors not changing the active-window result.

Darwin compilation checks the Objective-C signature, AppKit link, status
mapping, buffer ownership, and `pid_t` assertion. Manual verification covers
the actual `NSRunningApplication.icon` call because Linux CI cannot execute
AppKit.

### Dashboard

Template tests assert that app colours still match timeline bars, images have
empty alt text and fixed dimensions, missing icons render the deterministic
monogram, site rows are unchanged, and no inline CSS or external image source
is introduced.

## Security And Failure Behaviour

- The server performs no outbound request for an icon.
- Uploaded bytes are bounded and fully decoded as PNG before storage.
- Upload ownership comes only from authenticated device context.
- Upload requests are rate-limited per device before decode.
- Database rows and image reads are user-scoped.
- Image URLs contain opaque IDs and digests, not application names.
- The reporter and tracker maps are bounded to 128 app keys.
- No missing, rejected, or unavailable icon can block activity recording.

| Failure | Behaviour |
|---|---|
| AppKit returns no icon | Use dashboard fallback; retry after 30 seconds |
| Native conversion returns invalid PNG | Log once per negative-cache lifetime; keep activity |
| Reporter map is full | Tracker does not remember digest and retries on next poll |
| Server is unavailable | Keep icon in memory and retry with bounded exponential backoff |
| Daemon exits before upload | Lose icon metadata; derive it again after restart |
| Server rejects a permanent payload error | Isolate the batch; drop only matching rejected snapshots |
| Database prunes an active icon | Tracker re-announces it within 30 days |
| App icon changes | Digest and immutable image URL change |
| Another user requests the image ID | Return 404 |

## Deferred Work

### Firefox Favicons

Firefox favicon delivery remains outside Phase 6. Ordinary `favIconUrl` values
are remote URLs, and the extension cannot read their bytes without tracked-site
host permissions. Those permissions were not added.

Phase 8 supersedes the earlier prohibition on server fetching with an explicit
operator-visible privacy decision: the Trackkr server fetches bounded HTTPS
favicons for canonical site totals and caches both success and failure for one
year. See `phase8-site-favicons-plan.md` for that separate threat model and
implementation contract.

### Linux Application Icons

Linux application icons get a separate plan after the macOS storage and UI
contract ships. That plan should be written and manually exercised on a Linux
desktop so desktop-entry matching, icon themes, and source-image limits are
based on an environment that can actually run them.

### Site Normalization

The trailing DNS dot correction in `siteExpr` landed separately after Phase 6.
It merges URLs such as `example.com.` and `example.com` without coupling that
site-grouping correction to application icon delivery.

## Manual Verification

1. Switch among Safari, Firefox, Finder, Terminal, helper processes, and an app
   with no usable icon; confirm activity is unchanged and app rows show icon or
   fallback.
2. Deny Accessibility permission and disable `macos_read_titles`; confirm app
   icons still work while titles remain absent.
3. Stop the server, switch through several apps, restart it, and confirm the
   in-memory reporter queue uploads without affecting activity delivery.
4. Force-stop and restart the daemon before icon upload; confirm the next app
   observation re-derives the lost icon.
5. Replace or update an app and confirm the positive cache refreshes within
   five minutes.
6. Open a day recorded before this phase and confirm a newly learned icon
   decorates its existing app total without backfill.
7. Use two users and confirm neither can retrieve the other's opaque image ID.
8. Use two devices with the same app key and different fixture icons; confirm
   and document last-accepted-wins behaviour.

## Verification Commands

```sh
mise format
mise test
mise test-race
mise test-coverage
mise lint
mise portability
mise hooks
```

Coverage remains at or above 50 percent. Linux CI runs the shared, database,
HTTP, reporter, tracker, and template tests. `mise portability` keeps the
Darwin no-cgo fallback compiling. Native AppKit execution remains a documented
macOS check.

## External Contracts

- Apple `NSRunningApplication.icon` supplies the running application's icon:
  <https://developer.apple.com/documentation/appkit/nsrunningapplication/icon>
- Apple warns that `processIdentifier` is not a stable process identity:
  <https://developer.apple.com/documentation/appkit/nsrunningapplication/processidentifier>
- Mozilla documents `Tab.favIconUrl` as a URL exposed by the `tabs`
  permission:
  <https://developer.mozilla.org/en-US/docs/Mozilla/Add-ons/WebExtensions/API/tabs/Tab#faviconurl>
- Mozilla documents cross-origin `fetch` as a host-permission capability:
  <https://developer.mozilla.org/en-US/docs/Mozilla/Add-ons/WebExtensions/manifest.json/permissions#host_permissions>
