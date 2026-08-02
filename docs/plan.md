# Trackkr — Activity Tracking App

## Context

Build a cross-platform activity/time tracking app that monitors active windows and browser tabs, reports data to a central server, and presents it on a web dashboard. The user has multiple devices (macOS laptop, Linux desktop, Android phone) and wants a unified view of how they spend time.

---

## Architecture Overview

```text
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│ Linux Daemon │     │ macOS Daemon │     │ Android App  │
│  (trackkrd)  │     │  (trackkrd)  │     │  (future)    │
│ + Firefox /  │     │ + Firefox /  │     │              │
│    Chrome    │     │    Chrome    │     │              │
└──────┬───────┘     └──────┬───────┘     └──────┬───────┘
       │                    │                    │
       │  POST /api/v1/activity (batched)        │
       └────────────┬───────┴────────────────────┘
                    ▼
           ┌────────────────┐
           │  Go Server     │
           │  (API + Web)   │
           │  HTMX + tmpls  │
           └───────┬────────┘
                   │
                   ▼
           ┌────────────────┐
           │  PostgreSQL    │
           └────────────────┘
```

---

## Tech Stack

- **Server**: Go, chi router, pgx/v5, golang-migrate, bcrypt, zerolog
- **Dashboard**: Go html/template + HTMX 2.0.x and Bootstrap 5.3 CSS (both
  vendored, no Bootstrap JS), inline SVG timeline bars
- **Database**: PostgreSQL 18 (`postgres:18-alpine`, digest-pinned in docker-compose)
- **Desktop client**: Go daemon, platform-specific window/idle detection
- **Browser extensions**: Firefox and Google Chrome Manifest V3 extensions,
  talking to a token-authenticated loopback listener on 127.0.0.1:7600
- **Deployment**: Docker (multi-stage build), docker-compose for dev
- **Auto-start**: systemd (Linux), launchd (macOS)
- **Task runner**: mise

---

## Project Structure

```text
trackkr/
├── cmd/
│   ├── server/main.go              # Server entrypoint (API + web dashboard)
│   └── trackkrd/main.go            # Client daemon entrypoint (activity tracker)
├── internal/
│   ├── server/
│   │   ├── server.go               # HTTP server setup, middleware, routes
│   │   ├── auth.go                 # API key validation
│   │   ├── session.go              # Signed-cookie sessions
│   │   ├── middleware.go           # RequireSession, security headers, CSRF
│   │   ├── handlers_api.go         # API handlers (ingest, devices)
│   │   ├── app_icons.go            # App-icon upload, throttling, image delivery
│   │   ├── handlers_web.go         # Web dashboard handlers
│   │   ├── templates.go            # Template parsing + render helper
│   │   ├── timeline.go             # Record -> SVG bar geometry
│   │   └── config.go               # Server config (TOML file + env var overrides for secrets)
│   ├── db/
│   │   ├── db.go                   # Connection pool setup
│   │   ├── migrations/
│   │   │   ├── 001_initial.up.sql
│   │   │   ├── 001_initial.down.sql
│   │   │   ├── 002_device_cascade.up.sql
│   │   │   ├── 002_device_cascade.down.sql
│   │   │   ├── 003_app_icons.up.sql
│   │   │   └── 003_app_icons.down.sql
│   │   ├── queries.go              # Query functions
│   │   └── models.go               # DB model structs
│   ├── icon/
│   │   └── app.go                  # App-key and bounded PNG contract
│   ├── tracker/
│   │   ├── tracker.go              # Main tracking loop + idle state machine
│   │   ├── app_icon.go             # Portable icon cache and announcement state
│   │   ├── window.go               # ActiveWindow interface
│   │   ├── window_linux.go         # Linux: xdotool + xprop
│   │   ├── window_darwin.go        # macOS adapter: CoreGraphics + AX via cgo
│   │   ├── detector_core.go        # Portable active-app decision logic
│   │   ├── titles.go               # Portable Accessibility trust policy
│   │   ├── macos_darwin.m          # Objective-C boundary for app/title reads
│   │   ├── macos_darwin.h          # C contract shared by the .m and cgo
│   │   ├── platform_nocgo_darwin.go # darwin && !cgo: both factories
│   │   ├── idle.go                 # IdleDetector interface
│   │   ├── idle_linux.go           # Linux: xprintidle
│   │   ├── idle_darwin.go          # macOS: CoreGraphics HID idle time via cgo
│   │   ├── extension.go            # Loopback listener for the browser extension
│   │   ├── reporter.go             # Batch sender (queue + flush)
│   │   └── config.go               # Client config
│   └── models/
│       └── activity.go             # Shared types: ActivityRecord
├── web/                            # Go package: go:embed templates + static
│   ├── web.go
│   ├── templates/
│   │   ├── base.html
│   │   ├── login.html
│   │   ├── register.html
│   │   ├── dashboard.html
│   │   ├── devices.html
│   │   └── partials/
│   │       ├── timeline.html       # inline SVG chart
│   │       └── device_rows.html
│   └── static/
│       ├── bootstrap.min.css       # 5.3.x compiled CSS, no Bootstrap JS
│       ├── style.css
│       ├── htmx.min.js             # 2.0.x, exact version pinned
│       └── fonts/
├── extension/
│   ├── manifest.json               # Firefox MV3 source manifest
│   ├── manifest.chrome.json        # Chrome MV3 source manifest
│   ├── common.js                   # shared helpers
│   ├── background-core.js          # shared tab, focus, idle, queue logic
│   ├── background-fx.js            # Firefox event-page entrypoint
│   ├── background-cr.js            # Chrome service-worker entrypoint
│   ├── popup.html / popup.js / popup.css
│   ├── options.html / options.js / options.css
│   ├── tests/                      # shared Node harness and browser tests
│   └── icons/
├── deploy/
│   ├── trackkrd.service            # systemd unit
│   ├── Info.plist                  # macOS bundle template
│   └── README-macos.md             # install, permissions, launchctl, signing
├── scripts/
│   ├── dev.sh                      # whole stack: db, server, device, daemon
│   ├── bundle-macos.sh             # build, sign, and install the .app
│   ├── bundle-install.sh           # sourced: transactional bundle replacement
│   └── bundle-macos_test.sh        # runs on Linux CI, no codesign needed
├── docs/
│   ├── plan.md                     # This file
│   ├── phase2-plan.md              # Linux daemon design
│   ├── phase3-plan.md              # Web dashboard design
│   ├── phase4-plan.md              # Firefox extension design
│   ├── phase5-plan.md              # macOS support design
│   ├── phase6-icons-plan.md        # macOS application-icon design
│   ├── phase7-dedup-plan.md        # browser/desktop deduplication design
│   ├── phase8-site-favicons-plan.md # server-fetched favicon design
│   └── phase9-chrome-extension-plan.md # Chrome extension design
├── go.mod
├── go.sum
├── mise.toml                       # Task runner
├── .golangci.yml                   # Linter config
├── Dockerfile
└── docker-compose.yml
```

---

## Database Schema

```sql
CREATE TABLE users (
    id          BIGSERIAL PRIMARY KEY,
    username    TEXT NOT NULL UNIQUE,
    password    TEXT NOT NULL,  -- bcrypt hash
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE devices (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id),
    name        TEXT NOT NULL,
    device_type TEXT NOT NULL DEFAULT 'desktop',
    api_key     TEXT NOT NULL UNIQUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE activity_records (
    id          BIGSERIAL PRIMARY KEY,
    device_id   BIGINT NOT NULL REFERENCES devices(id),
    app_name    TEXT NOT NULL,
    title       TEXT NOT NULL DEFAULT '',
    url         TEXT,
    started_at  TIMESTAMPTZ NOT NULL,
    ended_at    TIMESTAMPTZ NOT NULL,
    duration_s  INTEGER NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (device_id, started_at)
);

CREATE INDEX idx_activity_records_started ON activity_records (started_at);

CREATE TABLE app_icons (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    app_key      TEXT NOT NULL,
    png          BYTEA NOT NULL,
    sha256       BYTEA NOT NULL,
    width        SMALLINT NOT NULL,
    height       SMALLINT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, app_key)
);
```

The `UNIQUE (device_id, started_at)` constraint makes re-sent batches
idempotent, and its backing index also serves device-filtered timeline
queries, so no separate `(device_id, started_at)` index is needed.

---

## API Endpoints

### Ingestion (API key auth via `X-API-Key` header)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/activity` | Submit batch of activity records |
| `POST` | `/api/v1/app-icons` | Submit validated macOS application icons |
| `GET` | `/api/v1/devices` | List devices for authenticated user |
| `POST` | `/api/v1/heartbeat` | Daemon liveness ping |

### Web Dashboard (session cookie auth)

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `GET/POST` | `/login` | public | Login page + form submit |
| `GET/POST` | `/register` | public | Signup, mounted only when `allow_registration` |
| `POST` | `/logout` | public | Clear session |
| `GET` | `/static/*` | public | Embedded CSS, JS, fonts |
| `GET` | `/` | session | Dashboard (today's timeline) |
| `GET` | `/timeline` | session | HTMX partial: timeline for date+device filter |
| `GET` | `/app-icons/{id}/{sha256}.png` | session | Immutable user-owned application icon |
| `GET/POST/DELETE` | `/devices` | session | Device management |

---

## Client Daemon Design

### Tracking Loop (polls every 3 seconds)

```text
                   ┌───────────────────────┐
         ┌────────►│   TRACKING            │◄──────────┐
         │         │ (current app/title)   │           │
         │         └─────┬────────┬────────┘           │
         │               │        │                    │
    window changed    idle > 5min │               input detected
    (emit record,        │        │               (discard idle,
     start new)          │        │               start new record)
         │               ▼        │                     │
         │         ┌──────────────┴──┐                  │
         └─────────│     IDLE        ├──────────────────┘
                   └─────────────────┘
```

- **Window OR title changes**: finalize current record, start new one (e.g., switching files in VS Code = new record)
- **Idle detected (5 min no input)**: finalize current record with `ended_at = now - 5min`, subtract 5 min
- **Resume from idle**: start fresh record from now

### Reporter (batch sender)

- In-memory queue, flushes every 30s or 20 records
- On network failure, records stay in queue and retry next cycle
- Persist queue to disk (`~/.local/share/trackkr/pending.json`) so records survive daemon restarts
- macOS application icons share the flush loop but use a bounded, best-effort
  memory map; activity flush and persistence always run first, while icon
  retries use bounded backoff and honour `Retry-After`

### Active Window Detection

- **Linux (X11)**: `xdotool` for window name, `xprop` for WM_CLASS (app name)
- **Linux (sway)**: sway's own IPC socket at `$SWAYSOCK`, spoken directly.
  `app_id` names native Wayland clients and `window_properties.class` names
  XWayland ones, so an application reports the same name on either session.
  A Wayland compositor that is not sway gets no window detection rather than
  `xdotool`, which under Wayland answers about whichever X client XWayland
  last saw focused
- **macOS**: `CGWindowListCopyWindowInfo` supplies the owner and pid of the
  frontmost layer-zero window. Accessibility reads its focused title only
  when trusted. AppKit derives a 64×64 application icon from the same pid
  without Accessibility permission, and app-name tracking continues if icon
  conversion or upload fails.

### Idle Detection

- **Linux (X11)**: `xprintidle` (returns idle ms)
- **Linux (Wayland)**: a supervised `swayidle` child. Wayland has no
  queryable idle time — a client can only ask to be told when a timeout it
  picks elapses — so the daemon runs one at its own threshold and converts
  the idle/resume pair back into a duration. Needs `swayidle` installed; a
  Wayland session without it reports no idle rather than falling back to
  `xprintidle`, which counts only the events XWayland itself sees
- **macOS**: CoreGraphics
  `CGEventSourceSecondsSinceLastEventType` for HID-system idle time

The daemon reads `SWAYSOCK` and `WAYLAND_DISPLAY` from its own environment,
so a systemd user unit needs `systemctl --user import-environment` (a sway
`exec` line inherits them). Both are recovered by scanning
`$XDG_RUNTIME_DIR` when a restarted compositor moves them.

The browser extension asks the daemon rather than deciding for itself, over
`GET /extension/idle`. `browser.idle` on Linux reads the same X screensaver
counter `xprintidle` does, so an extension trusting it keeps timing a tab
for as long as its user is away — one observed record ran 41 minutes past
the moment the desktop side correctly stopped. The daemon answers with
`idle_since`, the moment activity stopped, so a poll arriving late still
closes the segment in the right place. One source holds authority at a
time: a request that fails, answers non-2xx, or carries an unusable
timestamp hands it back to `browser.idle`, and the popup says which is in
use. A screen lock ends a segment immediately under either source, since
the user asked for it.

### Client Config (`os.UserConfigDir()/trackkr/config.toml`)

```toml
server_url = "https://trackkr.example.com"
api_key = "abc123..."
device_name = "work-laptop"
poll_interval = "3s"
idle_threshold = "5m"
macos_read_titles = true
macos_prompt_for_accessibility = false
```

---

## Browser Extensions

- Firefox talks to `/extension/activity`; Chrome talks to
  `/extension/activity/chrome`. Both use a
  bearer token from `trackkrd -print-extension-token`. "Localhost only" is
  not sufficient on its own: any local process can post to the port, and a
  visited web page can attempt a cross-origin write, so the listener also
  requires `Content-Type: application/json` (which forces a preflight it
  never answers) and accepts only parsed `moz-extension://` or
  `chrome-extension://` origins
- Both use Manifest V3 and request the daemon's loopback host permission at
  runtime. Chrome 142 and newer additionally require a foreground popup or
  options request to grant Local Network Access before its service worker can
  deliver
- Listens for `tabs.onActivated`, `tabs.onUpdated`, `windows.onFocusChanged`,
  and `idle.onStateChanged`, so leaving the browser or the desk both end the
  current segment as the desktop tracker's idle handling does
- Skips private windows entirely, along with `about:`, `file:`, and
  extension pages
- Sends the previous tab as a completed record when the tab changes; the
  unsent queue lives in `storage.local` so it survives a browser restart,
  while the in-flight tab lives in `storage.session` so a stale focus does
  not
- A shared core owns tracking and queue semantics. Firefox loads a thin event
  page entrypoint; Chrome loads a thin service-worker entrypoint with all
  listeners registered synchronously. Queue-first finalization and stable
  record IDs make worker termination replay-safe
- The route stamps the trusted browser producer and canonical application name
  before feeding the reporter. Dashboard queries subtract each browser's URL
  coverage only from matching desktop observations on the same device;
  desktop-only gaps remain visible. See `phase7-dedup-plan.md` and
  `phase9-chrome-extension-plan.md`.

---

## Dashboard MVP — Daily Timeline

- Inline SVG `<rect>` bars with `x`/`width` in minutes from midnight, inside a
  `viewBox` whose span is the real day length (23, 24, or 25 hours across DST)
- Geometry travels as presentation attributes, not inline CSS: a strict
  `style-src 'self'` survives HTMX swaps, where a nonce could not
- Colors assigned per app via deterministic hash
- App totals show an authenticated macOS application icon when available and
  a deterministic colour-matched monogram with luminance-selected text
  otherwise
- HTMX partials for date picker and device filter (swap timeline on change)
- Hover detail via SVG `<title>` plus a CSS-revealed label: app name, title,
  time range
- See `phase3-plan.md` for the full design

---

## Server Config (`config.toml`)

```toml
[server]
host = "0.0.0.0"
port = 8080

[database]
host = "localhost"
port = 5455              # docker-compose maps 5455 -> 5432
name = "trackkr"
user = "trackkr"
# password loaded from env var TRACKKR_DB_PASSWORD
sslmode = "disable"

[auth]
# session_secret loaded from env var TRACKKR_SESSION_SECRET
# allow_registration: set true to enable web-based user signup
allow_registration = false
```

Secrets (`database.password`, `auth.session_secret`) are loaded from
environment variables, keeping the config file committable. There is no
admin password hash: users live in the `users` table, created with
`trackkr-backend create-user <username> <password>`.

---

## Implementation Order

### Phase 1: Server Foundation (done)

1. Go module init, project structure, update mise.toml tasks
2. PostgreSQL connection with pgx/v5
3. Database migrations with golang-migrate
4. `POST /api/v1/activity` endpoint (batch ingest)
5. API key auth middleware
6. Test with curl

### Phase 2: Linux Desktop Client (done — see `phase2-plan.md`)

1. `window_linux.go` — active window via xdotool
2. `idle_linux.go` — idle time via xprintidle
3. Tracking loop with state machine
4. Reporter with batch sending, disk-persisted queue, graceful shutdown
5. End-to-end test: daemon → server → DB

### Phase 3: Web Dashboard MVP (done — see `phase3-plan.md`)

0. No heavy web-framework
1. Login page + session auth
2. Dashboard page with SVG timeline (Bootstrap 5.3 compiled CSS, no Bootstrap JS)
3. HTMX date/device filtering
4. Device management page (create device + API key)
5. Embed templates/static with Go embed

### Phase 4: Firefox Extension (done — see `phase4-plan.md`)

1. Daemon localhost endpoint on :7600
2. Extension background script with tab listeners
3. Popup showing connection status
4. Test tab tracking end-to-end
5. Per-site ignore rules, applied in the browser before anything is stored

Three behaviours still need a person driving Firefox: private windows,
revoking the host permission mid-session, and the idle threshold.

### Phase 5: macOS Support (implemented — see `phase5-plan.md`)

1. `idle_darwin.go` uses CoreGraphics and needs no permission
2. `window_darwin.go` records the frontmost visible window owner without
   permission and reads titles behind an Accessibility trust policy
3. `scripts/bundle-macos.sh` installs and signs a stable `.app` bundle path
4. A generated launchd user agent runs the bundled daemon in the GUI session

Three of the fourteen Verification steps in `phase5-plan.md` run in CI.
The other eleven need a Mac, and most need a person at its keyboard: the
Accessibility grant and the recheck that follows it, `launchctl
bootstrap` across a logout, rapid application switching, an application
with no windows, the lock screen, and whether a rebuild keeps the grant.
Nothing in CI substitutes for them.

Docker production packaging remains separate deployment polish.

The systemd unit, graceful shutdown, and disk-persisted queue originally
listed here all landed in Phase 2.

### Phase 6: macOS Application Icons (implemented — see `phase6-icons-plan.md`)

1. Shared normalized app-key and bounded PNG validation contract
2. User-scoped icon storage with serialized retention at 512 rows
3. Rate-limited device upload and authenticated immutable image routes
4. Best-effort delivery through the existing reporter loop
5. AppKit icon rendering, bounded cache, and dashboard monogram fallback

Firefox-delivered favicons remain out of scope for this phase. Phase 8 makes a
separate, explicit privacy trade-off by fetching site icons from the server.
Linux application icons still need a separately exercised Linux resolver.

### Phase 7: Desktop/Extension Deduplication (implemented — see `phase7-dedup-plan.md`)

1. Identify Firefox and Chrome sources by trusted producer and browser family
2. Merge extension coverage per device and browser family
3. Subtract only overlapping coverage from matching desktop browser records
4. Apply the same effective intervals to timeline records and app totals

### Phase 8: Server-Fetched Site Favicons (implemented — see `phase8-site-favicons-plan.md`)

1. User-scoped positive and negative favicon cache with one-year expiry
2. Signed authenticated image URLs derived from existing site totals
3. HTTPS-only, DNS-pinned fetching with strict SSRF and decode bounds
4. Bounded background fetching with private caching and monogram fallback
5. Serialized per-user retention at 2,048 rows

### Phase 9: Google Chrome Extension (implemented — see `phase9-chrome-extension-plan.md`)

1. Stable activity record identities and route-stamped browser producers
2. Browser-family-scoped deduplication, canonical totals, and icon aliases
3. Shared tracking core with Firefox and Chrome lifecycle entrypoints
4. Chrome MV3 service-worker packaging and clean allowlisted validation
5. Chrome 142+ Local Network Access onboarding and recovery guidance

The automated Node harness covers worker recreation with persistent extension
storage. Actual Chrome termination during in-flight finalization and the Local
Network Access prompt still require the manual acceptance checklist in the phase
plan.

### Phase 10: Future Enhancements (parked)

- Android app
- Wayland support
- Monthly views, categories, productivity scoring
- CSV and JSON data export

---

## Verification

1. **Server**: `curl -X POST -H "X-API-Key: ..." -d '{"records":[...]}' localhost:8080/api/v1/activity` returns 201
2. **Client**: Run daemon, switch windows, verify records appear in DB
3. **Dashboard**: Login, see today's timeline with colored blocks
4. **Extensions**: Install Firefox and the staged Chrome package, browse tabs in
   both, and verify URLs appear without cross-browser subtraction
5. **Docker**: `docker-compose up` starts server + postgres, dashboard accessible
