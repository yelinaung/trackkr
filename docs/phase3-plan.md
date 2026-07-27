# Phase 3: Web Dashboard MVP

## Context

Phases 1 and 2 are complete: the server ingests batched records at
`POST /api/v1/activity` behind API-key auth, and `trackkrd` reports window
activity to it. Nothing yet reads that data back out. Phase 3 adds the
session-authenticated web dashboard: login, a daily timeline, HTMX filtering,
and device management.

Much of the data layer already exists. `internal/db/queries.go` has
`GetActivityRecords`, `GetUserByUsername`, `CreateUser`, `ListDevicesByUser`,
`CreateDevice`, and `DeleteDevice`; `internal/server/config.go` already carries
`Auth.SessionSecret` and `Auth.AllowRegistration`. Phase 3 is mostly the HTTP
and rendering layer on top -- with two exceptions in step 5, where a lookup
by user ID is missing outright and the activity window predicate is wrong for
records spanning midnight.

## New Files

```
web/
  web.go                     # package web: go:embed templates + static
  templates/
    base.html                # shell: nav, flash area, block defs
    login.html
    register.html            # only routed when allow_registration
    dashboard.html
    devices.html
    partials/
      timeline.html          # HTMX target: inline SVG chart
      device_rows.html       # HTMX target after create/delete
  static/
    bootstrap.min.css        # vendored 5.3.x compiled CSS, no JS bundle
    style.css                # the project stylesheet: tokens, type, timeline
    htmx.min.js              # vendored, the only JavaScript on the page
    fonts/                   # self-hosted woff2 + OFL license
internal/server/
  session.go                 # signed-cookie sessions
  session_test.go
  middleware.go              # RequireSession, security headers, CSRF, limiter
  middleware_test.go
  handlers_web.go            # login, logout, dashboard, timeline, devices
  handlers_web_test.go
  templates.go               # parse at startup, render helper
  templates_test.go
  timeline.go                # pure layout math: records -> bars
  timeline_test.go
internal/db/
  migrations/002_device_cascade.up.sql / .down.sql
```

Modified: `internal/server/server.go` (routes, split Querier), `queries.go`
(app totals), `config.go` (timezone, secure-cookie flag), `config.toml`.

## Implementation Order

### Step 1: `web/web.go` -- embedded assets

`go:embed` patterns cannot escape the Go file's own directory, so the
repo-root `web/` tree becomes a small package that exports
`Templates embed.FS` and `Static embed.FS`. `internal/server` imports it.
This is the reason the plan does not put an embed directive in
`internal/server`.

### Step 2: `session.go` -- signed cookie sessions

Cookie value is `base64url(userID.expiryUnix) + "." +
base64url(HMAC-SHA256(secret, payload))`, verified with
`hmac.Equal`. No session table, no new dependency.

Parsing rejects `expiryUnix <= now` after verifying the signature. The
cookie's own `Expires` attribute is a client-side courtesy, not a control: an
attacker replays a captured value with `curl` and the browser is not
involved. Signature validity and freshness are separate checks and both
belong server-side, with a test that replays an expired-but-correctly-signed
cookie.

A weak secret makes cookies forgeable, so an empty or under-32-byte
`auth.session_secret` must stop startup rather than surface as a panic from
`session.go` on the first login. The server `Config` has no validation hook
at all today -- `LoadConfig` only decodes and applies env overrides -- so add
`(*Config).Validate() error` mirroring `internal/tracker/config.go`.

Where it is called matters: not from `LoadConfig`. `cmd/server/main.go` loads
the config before dispatching the `create-user` subcommand, so validating
there would make a database-only command demand a session secret it never
uses. Call it from `New` (step 8 makes `New` fallible anyway), so the rule is
"serving HTTP requires a session secret" rather than "existing requires one".

Cookie flags: `HttpOnly`, `SameSite=Lax`, `Path=/`, 7-day expiry, and
`Secure` when `server.secure_cookies` is true (default true; set false for
plain-HTTP local dev). Logout clears the cookie. There is no server-side
revocation in the MVP -- a stolen cookie stays valid until expiry, which is
the tradeoff for having no session store.

### Step 3: `middleware.go` -- auth gate, headers, CSRF

`RequireSession` resolves the cookie's `userID` through `GetUserByID`
(step 5) to a `*db.UserRow` and puts it in the request context (mirroring
`DeviceFromContext`), redirecting to `/login` on failure -- a deleted user
with a still-valid cookie must not stay logged in. HTMX requests get
`HX-Redirect` instead of a 302 so a partial swap does not paint a login page
inside a `<div>`.

Security headers, static for every response, with no nonce anywhere:

```
Content-Security-Policy:
  default-src 'self';
  script-src 'self';
  style-src 'self';
  img-src 'self' data:;
  font-src 'self';
  object-src 'none';
  base-uri 'none';
  form-action 'self';
  frame-ancestors 'none'
X-Content-Type-Options: nosniff
Referrer-Policy: same-origin
X-Frame-Options: DENY
```

An earlier draft had a per-response nonce authorizing an inline `<style>`
block of bar geometry. That cannot work with HTMX: a CSP header on the
`/timeline` partial response does not replace the containing document's
policy, so the swapped-in `<style>` carries a nonce the document's policy
never authorized, and the browser drops it. Reusing the document's nonce is
not a fix either -- CSP Level 3 requires a nonce to be unique per policy
transmission, and echoing a nonce the client sent back to us is exactly the
exfiltration path nonces are meant to close.

The resolution is step 7: emit no dynamic CSS at all. Bar geometry moves to
SVG `x`/`width`/`fill` presentation attributes, which are markup, not inline
style, and therefore need no CSP concession. `style-src 'self'` stays strict
and the middleware stays stateless. (Note that a nonce could never have
authorized `style="..."` attributes in the first place; nonces apply only to
`<style>` elements.)

CSRF: `SameSite=Lax` already blocks cross-site POSTs, with a double-submit
token behind it. A session-derived token cannot cover `POST /login` and
`POST /register`, which run before any session exists, so use one mechanism
everywhere instead of two: a `trackkr_csrf` cookie holding 32 random bytes,
set on any GET that renders a form, with the same value rendered into a
hidden field (and sent via `hx-headers` for `hx-delete`). State-changing
requests compare cookie against field with `hmac.Equal`. It carries the same
flags as the session cookie -- `HttpOnly` (the server renders the field, so
no script needs to read it), `SameSite=Lax`, `Path=/`, and `Secure` gated on
`server.secure_cookies` -- since a CSRF token sent in clear over HTTP while
the session cookie is protected defeats the point. Rotate the token on
successful login so a pre-auth value cannot be pinned into the authenticated
session.

### Step 4: Split `Querier`

`internal/server/server.go` has a single `Querier` field holding the two
ingest methods, and `unitServer` in `testhelper_test.go` builds a `Server`
with a `mockQuerier` in that field before calling `newRouter`. Widening that
one interface to cover the eight new methods would force `mockQuerier` to
implement all ten -- the opposite of the goal.

So `Server` gets separate narrow fields instead of one composite:

```go
type Server struct {
    api      APIQuerier      // GetDeviceByAPIKey, InsertActivityRecords
    sessions SessionQuerier   // GetUserByID
    web      WebQuerier       // users, devices, activity, totals
    ...
}
```

`New` fills all three from the same `*db.Queries`, which satisfies each.
Handlers keep taking the narrowest interface they need as a parameter, the
way `HandleIngestActivity(queries Querier)` already does, so a test can
either call a handler directly with a two-method fake or build a `Server`
populating only the fields whose routes it exercises. `mockQuerier` stays
two methods; web tests get their own fakes.

Two consequences that are easy to miss because they are not in a handler:

- `APIKeyAuth(s.queries)` in `newRouter` narrows too. Its parameter type
  becomes `APIQuerier`; leaving it as the vanished `Querier` will not
  compile.
- `unitServer` in `testhelper_test.go` builds `Server{queries: mock}` and
  then calls `newRouter`. That field is gone after this step, so every
  existing API test stops compiling even though none of them touch web
  routes. Migrate the helper to populate `api: mock` only, leaving
  `sessions` and `web` nil -- routes those tests never exercise. Web tests
  add their own helper populating the fields they need.

### Step 5: `queries.go` -- session lookup, overlap windows, totals

Three query changes, two of them corrections to existing behaviour:

1. Add `GetUserByID(ctx, id) (*UserRow, error)`. Only `GetUserByUsername`
   exists today, and `RequireSession` has a numeric `userID` from the
   cookie, not a username.

2. Change `GetActivityRecords` from `started_at >= $2 AND started_at < $3`
   to overlap semantics: `started_at < end AND ended_at > start`. The
   current predicate means a record that began at 23:50 and ended at 00:20
   is invisible on the second day, so the layout code's midnight clamping
   (step 7) would never see a record to clamp. Overlap selection is what
   makes that clamp reachable.

3. Add `GetAppTotals(ctx, userID, start, end, deviceID) ([]AppTotalRow,
   error)` -- but summing `duration_s` over overlapping rows overcounts, by
   attributing a boundary record's whole span to both days. Sum the
   intersected span instead:

```sql
SELECT ar.app_name,
       SUM(EXTRACT(EPOCH FROM (LEAST(ar.ended_at, $3) - GREATEST(ar.started_at, $2))))
FROM activity_records ar
JOIN devices d ON d.id = ar.device_id
WHERE d.user_id = $1 AND ar.started_at < $3 AND ar.ended_at > $2
GROUP BY ar.app_name
ORDER BY 2 DESC
```

The `JOIN devices` and `d.user_id = $1` are not incidental: without them the
totals span every user in the database. `GetActivityRecords` already scopes
this way, and step 9 exists because the same class of mistake leaks data, so
the predicate is written out here rather than left implied. Follow the
existing shape in `GetActivityRecords` and duplicate the whole query string
across a device-filtered branch (`AND ar.device_id = $4`) and an unfiltered
one, rather than concatenating SQL -- it is the established pattern in this
file and keeps the placeholder numbering obvious.

`GetActivityRecords` takes `LIMIT 5000` with
`ORDER BY started_at, device_id, id` -- fully deterministic, so the same day
renders identically on every request rather than shuffling within equal
timestamps. The cap truncates the evening rather than sampling the day, and
the totals are a separate aggregate over the whole window, so a truncated
chart would otherwise disagree with its own summary. The handler therefore
compares the row count against the limit and the page states plainly that the
chart is truncated while the totals remain complete.

Changing an existing query means the existing `internal/db/queries_test.go`
cases need extending with a crosses-midnight fixture asserted from both days.

Indexing is acceptable at this scale, not optimal. The
`UNIQUE (device_id, started_at)` index bounded the old
`started_at >= $2 AND started_at < $3` predicate on both sides; the overlap
form bounds only the upper side, since `ended_at` is unindexed, so a
device-filtered day scans index entries from the beginning of history up to
`end` and filters `ended_at` per row. For one person's data that is
irrelevant, and the row cap contains it. If it ever matters, the cheap fix is
a sargable lower bound (`started_at > $2 - interval '1 day'`, valid only
while no record can outlast that window) or an index on
`(device_id, ended_at)`.

### Step 6: `migrations/002_device_cascade` -- make delete work

`activity_records.device_id` references `devices(id)` with no
`ON DELETE` clause, so `DeleteDevice` fails with a foreign-key violation for
any device that has ever reported. Migration 002 drops and re-adds the
constraint with `ON DELETE CASCADE`. Without this, the device management page
ships broken for exactly the devices a user would want to remove.

That failure is currently asserted, not demonstrated: the existing
`TestDeleteDevice` deletes a device with no records, so it never trips the
constraint, and a migration that silently did nothing would still leave the
suite green. Write the failing case first -- a device with records, deleted
against migration 001, expecting the FK violation -- watch it fail for the
right reason, then add 002 and convert it into a cascade assertion that the
device and its records are both gone.

### Step 7: `timeline.go` -- SVG geometry, DST-correct day length

Pure function: `layout(records, dayStart) Chart`, where each `Bar` carries
`X`, `Width`, `Fill`, `AppName`, `Title`, and `TimeRange`. The template
renders `<rect x= width= fill=>` inside a `viewBox`, so geometry travels as
markup rather than CSS -- which is what lets the CSP in step 3 stay strict
through HTMX swaps. Hover detail is a `<title>` child per rect for the native
tooltip, with a richer CSS-only label revealed via `rect:hover + g.label`;
both are presentation attributes and stylesheet rules, no inline CSS.

Units are minutes from `dayStart`, not percentages, and the day length is
computed, never assumed:

```go
dayEnd := dayStart.AddDate(0, 0, 1)   // not dayStart.Add(24 * time.Hour)
span  := dayEnd.Sub(dayStart)          // 23h, 24h, or 25h
```

`AddDate` on a zoned time yields the correct wall-clock next midnight across
a DST boundary, where adding 24 hours does not. `viewBox` width is
`span.Minutes()`, so a 23-hour spring-forward day renders 1380 units and a
25-hour fall-back day 1500 -- and the hour gridlines follow the same span
rather than a hardcoded 24. Table tests cover both transitions in a
DST-observing zone plus a UTC control.

Bars clamp to `[dayStart, dayEnd)`, which is what makes the overlap query in
step 5 useful, and enforce a minimum width of one minute so a short record
stays visible and hoverable. Colour comes from an FNV-1a hash of `app_name`
mapped to a hue.

One lane per device, plus a merged "All devices" view driven by the existing
`deviceID *int64` filter.

### Step 8: `templates.go` + `handlers_web.go` -- pages

Templates are parsed once at startup, not per request, as one set per full
page: base plus that page's template plus the shared func map, so
`{{define}}` blocks cannot collide across pages. HTMX partials are their own
sets with no base, since they render fragments rather than documents.

Parse failure has to reach the operator, and `New` currently returns only
`*Server`, so it becomes `New(...) (*Server, error)` -- `cmd/server/main.go`
fatals on it, and `unitServer` handles the extra return. That signature
change also gives `Config.Validate` (step 2) somewhere to be called.

Routes, and which side of `RequireSession` each sits on. Getting this wrong
is the most likely first-implementation bug, and the failure is quiet: gate
`/static/*` and the login page loads with no CSS at all, with no CDN fallback
under `style-src 'self'`.

```
public   GET /login   POST /login   GET /register   POST /register
         POST /logout                GET /static/*
gated    GET /        GET /timeline
         GET /devices  POST /devices  DELETE /devices/{id}
         (all /api/v1/* stays on APIKeyAuth, unchanged)
```

`POST /logout` is public deliberately: clearing a cookie for someone whose
session already expired should redirect to the login page, not 401. Register
routes are only mounted when `allow_registration` is set.

`GET /login` and `GET /register` issue the anonymous CSRF cookie from step 3
and render its value as a hidden field, so the pre-session POSTs are covered
by the same double-submit check as everything else. On success, login rotates
both the CSRF token and the session cookie.

Login compares with bcrypt and runs a dummy compare when the username is
unknown, so response timing does not leak which accounts exist; the error
message stays a generic "invalid username or password".

The attempt limiter needs specifying, not hand-waving, because the obvious
implementation does nothing: `r.RemoteAddr` is `host:port` with an ephemeral
port, so keying on it directly gives every request a fresh bucket. Key on
`host` from `net.SplitHostPort` (falling back to the raw value if it does not
parse), and deliberately not on `X-Forwarded-For`: chi's `RealIP` middleware
was dropped from this router in `d27e331` as spoofable, and a header-derived
limiter key is the same header-forged bypass wearing a different hat. Concretely: 10 failed attempts per host per 15-minute sliding
window, then `429` until the window drains; successes clear the bucket; a
`sync.Mutex`-guarded map of host to timestamps, swept on write so stale
hosts are evicted and the map cannot grow without bound. Behind a proxy this
limits per proxy IP, which is the honest consequence of not trusting
forwarding headers.

Device creation uses `GenerateAPIKey` (already in `auth.go`) and lists keys
with a copy button. Keys are stored in plaintext by design in the current
schema, so displaying them is honest rather than a leak; hashing them is a
Phase 6 change with its own migration.

### Step 9: `GET /api/v1/devices`

`plan.md` documents this endpoint and it was never built. It reads through
`ListDevicesByUser`, scoped to the authenticated device's user.

It must not marshal `db.DeviceRow` directly: that struct carries the
plaintext `APIKey`, so one compromised device key would harvest every other
key on the account -- lateral movement handed over by the API. Respond with
an explicit DTO of `id`, `name`, `device_type`, `created_at` and nothing
else, and add a test asserting no key material appears in the body. The
session-authenticated `/devices` page still shows keys on purpose; that is a
different trust level, reached with a password rather than a device
credential.

## Front-End Stack

Three files, no build step, no npm, everything vendored rather than linked:

1. `bootstrap.min.css` -- Bootstrap 5.3.x compiled CSS only.
2. `style.css` -- one substantial project-owned stylesheet, loaded after
   Bootstrap. This is where the design lives, not a handful of overrides.
3. `htmx.min.js` -- the only JavaScript on any page.

**No Bootstrap JavaScript.** That is a deliberate constraint, and it rules
out every Bootstrap component that is JS-driven, so the plan has to route
around them rather than discover them one at a time:

- Navbar collapse: no toggler. Use a flex nav that wraps on small screens, or
  a CSS-only `:checked` disclosure. No hamburger that needs JS.
- Modals: none. Device deletion uses `hx-confirm` (the browser's own dialog);
  anything richer would need `<dialog>` plus a few lines of vanilla JS, which
  is out of scope here.
- Tooltips and popovers: markup and CSS only. Timeline bars use an SVG
  `<title>` plus a CSS-revealed label group (step 7); Bootstrap's tooltip is
  a JS component and is not available.
- Dismissible alerts: flash messages render as plain, non-dismissible alerts.
- Dropdowns, offcanvas, toasts, collapse, scrollspy, carousel: not used.
- Color modes: Bootstrap 5.3 ships dark mode as a JS-toggled
  `data-bs-theme` attribute. Without JS, `style.css` maps the `--bs-*`
  tokens under `prefers-color-scheme: dark` instead. A user-chosen override
  would need a server-side cookie and is deferred.

The style layer treats Bootstrap as a reset, grid, and form primitive, then
sets its own tokens on top: an explicit palette (no purple-on-white,
legible in both schemes), a display/text/mono font trio self-hosted as woff2
under `static/fonts` with the OFL alongside, a layered rather than flat
background, and motion limited to a timeline-bar reveal on load plus a
crossfade on HTMX swap. The reveal staggers by coarse `nth-child` buckets --
twelve cycling rules in `style.css`, not a per-bar `animation-delay`. A true
per-bar delay would need a `style` attribute on every rect, which is the
inline CSS `style-src 'self'` forbids, and `attr()` is not usably supported
for non-content values. (SVG SMIL `<animate begin="...">` would also work,
being markup like the geometry, but the bucket approach needs no SMIL
support.) Default Bootstrap is exactly the
interchangeable look `AGENTS.md` warns against, so the theme is what makes
this ours. The timeline scrolls horizontally on narrow screens instead of
collapsing.

Every rule lives in `style.css` -- with one exception the plan has to close,
because HTMX itself injects a `<style>` element on load for its request
indicators. Under `style-src 'self'` that is a CSP violation on every page,
not a corner case, so `base.html` configures HTMX before it loads:

```html
<meta name="htmx-config"
      content='{"includeIndicatorStyles":false,"allowEval":false,"allowScriptTags":false}'>
```

`includeIndicatorStyles:false` suppresses the injected block, and the
`.htmx-indicator` rules move into `style.css` where the rest of the design
already lives. `allowEval:false` and `allowScriptTags:false` are not required
by the CSP but cost nothing and shrink the attack surface of swapped-in
markup.

This makes the HTMX version load-bearing rather than incidental: the config
key is spelled `includeIndicatorStyles` in HTMX 2 and is renamed in 4, so
pin the exact version (`2.0.x`) in the vendored file's header comment and in
`plan.md`, and re-check this flag whenever it is bumped. Bootstrap is pinned
the same way. Renovate does not track vendored assets, so refreshing either
is a manual chore worth calling out in the commit that adds them.

## Key Design Decisions

- No session store: signed cookies keep the dependency list at chi + pgx +
  bcrypt + zerolog, at the cost of no revocation before expiry. Expiry is
  enforced when parsing, not by the cookie attribute.
- SVG geometry over inline CSS: a nonce cannot survive an HTMX swap, since
  the partial's CSP header does not replace the document's policy, and
  reusing the document nonce breaks CSP Level 3's uniqueness requirement.
  Presentation attributes need no CSP concession, so `'unsafe-inline'` never
  enters the picture.
- One CSRF mechanism (cookie plus hidden field) rather than a session-derived
  token, because login and register have no session to derive from.
- Layout math in Go, not CSS: testable, and the midnight-crossing case is a
  bug magnet -- which is also why the query moves to overlap semantics, so
  such a record is actually selected on both days.
- Day length from `AddDate(0, 0, 1)`, never `Add(24 * time.Hour)`: DST days
  are 23 or 25 hours and the chart span has to match.
- Three narrow fields on `Server` (`api`, `sessions`, `web`) rather than one
  widened `Querier`, so a fake only implements what its routes touch.
- Server-side timezone (`server.timezone`, `TRACKKR_TIMEZONE`, default
  `Local`) resolved with `time.LoadLocation`. Records are `TIMESTAMPTZ`, so
  "today" is meaningless without a zone; per-user zones are Phase 6.

## Non-Goals

Weekly and monthly views, categories, productivity scoring, data export,
per-user timezones, hashed API keys, and server-side session revocation all
stay parked in Phase 6.

## Verification

1. `mise test` and `mise test-race` pass; coverage stays at or above 50%.
2. `mise lint` clean; `mise hooks` clean.
3. `mise db`, `mise run`, then `trackkr-backend create-user` (already
   implemented) and log in at `http://localhost:8080/login`.
4. Timeline shows coloured blocks for a day with real daemon data; date and
   device filters swap only the partial (verify in the network panel).
5. Create a device, copy its key into a daemon config, confirm records land;
   delete that device and confirm the cascade removes its records rather than
   erroring.
6. `curl -H "X-API-Key: ..." localhost:8080/api/v1/devices` returns the
   device list with no `api_key` field in the body.
7. Load the dashboard at mobile and desktop widths, then change the date
   filter; confirm the console shows no CSP violation on the swapped-in
   partial, which is where the nonce design failed.
8. Insert a record spanning 23:50 to 00:20 and confirm it appears on both
   days, clamped, and that each day's app total counts only its own share.
9. Fail login 11 times from the same host and confirm the 11th is throttled,
   then confirm a success clears the bucket.
10. Delete a user row while holding a valid cookie; the next request must
    redirect to `/login` rather than 500.
11. Replay a correctly signed but expired session cookie with `curl`; it must
    be rejected.
12. POST to `/login` without the CSRF field; it must be rejected.
13. Render a spring-forward and a fall-back day in a DST zone; the chart span
    must be 23 and 25 hours, with matching gridline counts.
