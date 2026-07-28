# Trackkr — Activity Tracking App

## Context

Build a cross-platform activity/time tracking app that monitors active windows and browser tabs, reports data to a central server, and presents it on a web dashboard. The user has multiple devices (macOS laptop, Linux desktop, Android phone) and wants a unified view of how they spend time.

---

## Architecture Overview

```
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│ Linux Daemon │     │ macOS Daemon │     │ Android App  │
│  (trackkrd)  │     │  (trackkrd)  │     │  (future)    │
│  + Firefox   │     │  + Firefox   │     │              │
│   Extension  │     │   Extension  │     │              │
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
- **Browser extension**: Firefox WebExtension (Manifest V3), talks to a
  token-authenticated loopback listener on 127.0.0.1:7600
- **Deployment**: Docker (multi-stage build), docker-compose for dev
- **Auto-start**: systemd (Linux), launchd (macOS)
- **Task runner**: mise

---

## Project Structure

```
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
│   │   │   └── 002_device_cascade.down.sql
│   │   ├── queries.go              # Query functions
│   │   └── models.go               # DB model structs
│   ├── tracker/
│   │   ├── tracker.go              # Main tracking loop + idle state machine
│   │   ├── window.go               # ActiveWindow interface
│   │   ├── window_linux.go         # Linux: xdotool + xprop
│   │   ├── window_darwin.go        # macOS: CGWindow via cgo
│   │   ├── idle.go                 # IdleDetector interface
│   │   ├── idle_linux.go           # Linux: xprintidle
│   │   ├── idle_darwin.go          # macOS: IOKit via cgo
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
│   ├── manifest.json               # MV3
│   ├── common.js                   # shared helpers
│   ├── background.js               # tab, focus, and idle listeners
│   ├── popup.html / popup.js / popup.css
│   ├── options.html / options.js / options.css
│   └── icons/
├── deploy/
│   ├── trackkrd.service            # systemd unit
│   └── com.trackkr.daemon.plist    # launchd plist
├── docs/
│   ├── plan.md                     # This file
│   ├── phase2-plan.md              # Linux daemon design
│   ├── phase3-plan.md              # Web dashboard design
│   └── phase4-plan.md              # Firefox extension design
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
| `GET` | `/api/v1/devices` | List devices for authenticated user (planned, Phase 3) |
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
| `GET/POST/DELETE` | `/devices` | session | Device management |

---

## Client Daemon Design

### Tracking Loop (polls every 3 seconds)

```
                    ┌──────────────────────┐
         ┌────────►│   TRACKING           │◄──────────┐
         │         │ (current app/title)   │           │
         │         └──────┬───────┬────────┘           │
         │                │       │                    │
    window changed    idle > 5min │              input detected
    (emit record,        │       │              (discard idle,
     start new)          │       │               start new record)
         │               ▼       │                     │
         │         ┌─────────────┴──┐                  │
         └─────────│     IDLE       ├──────────────────┘
                   └────────────────┘
```

- **Window OR title changes**: finalize current record, start new one (e.g., switching files in VS Code = new record)
- **Idle detected (5 min no input)**: finalize current record with `ended_at = now - 5min`, subtract 5 min
- **Resume from idle**: start fresh record from now

### Reporter (batch sender)
- In-memory queue, flushes every 30s or 20 records
- On network failure, records stay in queue and retry next cycle
- Persist queue to disk (`~/.local/share/trackkr/pending.json`) so records survive daemon restarts

### Active Window Detection
- **Linux**: `xdotool` for window name, `xprop` for WM_CLASS (app name)
- **macOS**: `CGWindowListCopyWindowInfo` via cgo (or `osascript` fallback)

### Idle Detection
- **Linux**: `xprintidle` (returns idle ms)
- **macOS**: IOKit `kHIDIdleTimeKey` via cgo

### Config (`~/.config/trackkr/config.toml`)
```toml
server_url = "https://trackkr.example.com"
api_key = "abc123..."
device_name = "work-laptop"
poll_interval = "3s"
idle_threshold = "5m"
```

---

## Firefox Extension

- Talks to the daemon at `http://127.0.0.1:7600/extension/activity`, with a
  bearer token from `trackkrd -print-extension-token`. "Localhost only" is
  not sufficient on its own: any local process can post to the port, and a
  visited web page can attempt a cross-origin write, so the listener also
  requires `Content-Type: application/json` (which forces a preflight it
  never answers) and rejects any `Origin` that is not `moz-extension://`
- Manifest V3, so the background script is a non-persistent event page and
  Firefox treats host permissions as optional — the extension requests the
  daemon origin at runtime and the popup reports when it is missing
- Listens for `tabs.onActivated`, `tabs.onUpdated`, `windows.onFocusChanged`,
  and `idle.onStateChanged`, so leaving the browser or the desk both end the
  current segment as the desktop tracker's idle handling does
- Skips private windows entirely, along with `about:`, `file:`, and
  extension pages
- Sends the previous tab as a completed record when the tab changes; the
  unsent queue lives in `storage.local` so it survives a browser restart,
  while the in-flight tab lives in `storage.session` so a stale focus does
  not
- Daemon enriches with `app_name = "Firefox"` and feeds into the reporter
  queue. The desktop tracker separately records `firefox` from WM_CLASS, so
  focused browsing is counted twice until Phase 6 deduplicates; the case
  difference keeps the two visible as distinct rows

---

## Dashboard MVP — Daily Timeline

- Inline SVG `<rect>` bars with `x`/`width` in minutes from midnight, inside a
  `viewBox` whose span is the real day length (23, 24, or 25 hours across DST)
- Geometry travels as presentation attributes, not inline CSS: a strict
  `style-src 'self'` survives HTMX swaps, where a nonce could not
- Colors assigned per app via deterministic hash
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

### Phase 3: Web Dashboard MVP (planned — see `phase3-plan.md`)
0. No heavy web-framework
1. Login page + session auth
2. Dashboard page with SVG timeline (Bootstrap 5.3 compiled CSS, no Bootstrap JS)
3. HTMX date/device filtering
4. Device management page (create device + API key)
5. Embed templates/static with Go embed

### Phase 4: Firefox Extension (in progress — see `phase4-plan.md`)
1. Daemon localhost endpoint on :7600
2. Extension background script with tab listeners
3. Popup showing connection status
4. Test tab tracking end-to-end

### Phase 5: macOS Support + Polish
1. `window_darwin.go` + `idle_darwin.go` via cgo
2. launchd plist for auto-start
3. Dockerfile + docker-compose for production

The systemd unit, graceful shutdown, and disk-persisted queue originally
listed here all landed in Phase 2.

### Phase 6: Future Enhancements (parked)
- Android app
- Wayland support
- Desktop/extension record deduplication
- Weekly/monthly views, categories, productivity scoring
- Data export

---

## Verification

1. **Server**: `curl -X POST -H "X-API-Key: ..." -d '{"records":[...]}' localhost:8080/api/v1/activity` returns 201
2. **Client**: Run daemon, switch windows, verify records appear in DB
3. **Dashboard**: Login, see today's timeline with colored blocks
4. **Extension**: Install in Firefox, browse tabs, verify URLs appear in timeline
5. **Docker**: `docker-compose up` starts server + postgres, dashboard accessible
