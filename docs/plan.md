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
- **Dashboard**: Go html/template + HTMX (vendored), pure CSS timeline bars
- **Database**: PostgreSQL 16
- **Desktop client**: Go daemon, platform-specific window/idle detection
- **Browser extension**: Firefox WebExtension (Manifest V2), talks to local daemon on localhost:7600
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
│   │   ├── auth.go                 # API key validation + session auth
│   │   ├── handlers_api.go         # API handlers (ingest, devices)
│   │   ├── handlers_web.go         # Web dashboard handlers
│   │   └── config.go               # Server config (TOML file + env var overrides for secrets)
│   ├── db/
│   │   ├── db.go                   # Connection pool setup
│   │   ├── migrations/
│   │   │   ├── 001_initial.up.sql
│   │   │   └── 001_initial.down.sql
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
├── web/
│   ├── templates/
│   │   ├── base.html
│   │   ├── login.html
│   │   ├── dashboard.html
│   │   └── partials/
│   │       ├── timeline.html
│   │       └── device_filter.html
│   └── static/
│       ├── style.css
│       └── htmx.min.js
├── extension/
│   ├── manifest.json
│   ├── background.js
│   ├── popup.html
│   ├── popup.js
│   └── icons/
├── deploy/
│   ├── trackkrd.service            # systemd unit
│   └── com.trackkr.daemon.plist    # launchd plist
├── docs/
│   └── plan.md                     # This file
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
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_activity_records_device_started ON activity_records (device_id, started_at);
CREATE INDEX idx_activity_records_started ON activity_records (started_at);
```

---

## API Endpoints

### Ingestion (API key auth via `X-API-Key` header)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/activity` | Submit batch of activity records |
| `GET` | `/api/v1/devices` | List devices for authenticated user |
| `POST` | `/api/v1/heartbeat` | Daemon liveness ping |

### Web Dashboard (session cookie auth)

| Method | Path | Description |
|--------|------|-------------|
| `GET/POST` | `/login` | Login page + form submit |
| `POST` | `/logout` | Clear session |
| `GET` | `/` | Dashboard (today's timeline) |
| `GET` | `/timeline` | HTMX partial: timeline for date+device filter |
| `GET/POST/DELETE` | `/devices` | Device management |

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

- Talks to local daemon on `http://localhost:7600/extension/activity` (no auth needed, localhost only)
- Listens for `tabs.onActivated` and `tabs.onUpdated`
- Skips incognito tabs
- Sends previous tab as a completed record when tab changes
- Daemon enriches with `app_name = "Firefox"` and feeds into reporter queue

---

## Dashboard MVP — Daily Timeline

- Pure CSS positioned `<div>` bars (percentage-based `left` and `width` within a 24h container)
- Colors assigned per app via deterministic hash
- HTMX partials for date picker and device filter (swap timeline on change)
- Hover tooltip shows app name, title, time range

---

## Server Config (`config.toml`)

```toml
[server]
host = "0.0.0.0"
port = 8080

[database]
host = "localhost"
port = 5432
name = "trackkr"
user = "trackkr"
# password loaded from env var TRACKKR_DB_PASSWORD
sslmode = "disable"

[auth]
# admin_password_hash loaded from env var TRACKKR_ADMIN_PASSWORD_HASH
session_secret = ""  # or from env var TRACKKR_SESSION_SECRET
```

Secrets (`database.password`, `auth.admin_password_hash`, `auth.session_secret`) are loaded from environment variables, keeping the config file committable.

---

## Implementation Order

### Phase 1: Server Foundation
1. Go module init, project structure, update mise.toml tasks
2. PostgreSQL connection with pgx/v5
3. Database migrations with golang-migrate
4. `POST /api/v1/activity` endpoint (batch ingest)
5. API key auth middleware
6. Test with curl

### Phase 2: Linux Desktop Client
1. `window_linux.go` — active window via xdotool
2. `idle_linux.go` — idle time via xprintidle
3. Tracking loop with state machine
4. Reporter with batch sending
5. End-to-end test: daemon → server → DB

### Phase 3: Web Dashboard MVP
No heavy web-framework
1. Login page + session auth
2. Dashboard page with CSS timeline (getbootstrap.com)
3. HTMX date/device filtering
4. Device management page (create device + API key)
5. Embed templates/static with Go embed

### Phase 4: Firefox Extension
1. Daemon localhost endpoint on :7600
2. Extension background script with tab listeners
3. Popup showing connection status
4. Test tab tracking end-to-end

### Phase 5: macOS Support + Polish
1. `window_darwin.go` + `idle_darwin.go` via cgo
2. launchd plist for auto-start
3. Dockerfile + docker-compose for production
4. systemd unit for Linux auto-start
5. Graceful shutdown, queue persistence

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
