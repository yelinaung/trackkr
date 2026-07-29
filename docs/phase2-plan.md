# Phase 2: Linux Desktop Client (`trackkrd`)

## Context

The server foundation (Phase 1) is complete — `POST /api/v1/activity` accepts batched activity records with API key auth. Phase 2 builds the Linux daemon that monitors active windows, detects idle time, and reports activity to the server.

## New Files

```text
internal/tracker/
├── config.go              # Client config (TOML + env overrides)
├── config_test.go
├── window.go              # WindowDetector interface + WindowInfo type
├── window_linux.go        # X11 impl: xdotool + xprop
├── window_linux_test.go
├── idle.go                # IdleDetector interface + NopIdleDetector
├── idle_linux.go          # xprintidle impl + graceful fallback
├── idle_linux_test.go
├── reporter.go            # Batch queue, HTTP POST, disk persistence
├── reporter_test.go
├── tracker.go             # State machine: TRACKING ↔ IDLE
├── tracker_test.go
cmd/trackkrd/main.go       # Daemon entry point
deploy/trackkrd.service     # systemd user unit
```

Modified: `mise.toml` (add `build-daemon`, `run-daemon` tasks)

## Implementation Order

### Step 1: `config.go` — Client configuration

- Config struct with TOML tags, defaults, env var overrides (`TRACKKR_SERVER_URL`, `TRACKKR_API_KEY`)
- Custom `duration` type implementing `encoding.TextUnmarshaler` for `"3s"`, `"5m"` strings
- Defaults: `poll_interval=3s`, `idle_threshold=5m`, `flush_interval=30s`, `flush_size=20`
- `DefaultConfigPath()` → `~/.config/trackkr/config.toml`
- `DefaultDataDir()` → `~/.local/share/trackkr/`

### Step 2: `window.go` + `window_linux.go` — Active window detection

- Interface: `WindowDetector` with `ActiveWindow() (WindowInfo, error)`
- `XWindowDetector` uses `exec.LookPath` for xdotool/xprop at construction
- `xdotool getactivewindow` → window ID, `getwindowname` → title
- `xprop -id <id> WM_CLASS` → parsed for app name (second value)
- Sentinel `ErrNoActiveWindow` for locked screen / no focus
- Unexported `parseWMClass(output string) string` for testability

### Step 3: `idle.go` + `idle_linux.go` — Idle detection

- Interface: `IdleDetector` with `IdleTime() (time.Duration, error)`
- `NopIdleDetector` always returns 0 (fallback when xprintidle missing)
- `XIdleDetector` runs xprintidle, parses ms output
- Factory: `NewIdleDetectorOrNop(logger)` — logs warning if xprintidle not found, returns Nop

### Step 4: `reporter.go` — Batch sender + persistence

- `Record` struct matching the server's ingest format
- `HTTPPoster` interface (`Do(*http.Request) (*http.Response, error)`) — `*http.Client` satisfies it
- In-memory queue protected by `sync.Mutex`
- `Run(ctx)`: select on flush ticker (30s), flush channel (threshold), ctx.Done
- `flush`: POST JSON to `{server_url}/api/v1/activity` with `X-API-Key` header; on failure, records stay in queue
- `loadPending` / `savePending`: JSON file at `{data_dir}/pending.json`, atomic write via temp+rename
- `Shutdown`: final flush + save pending

### Step 5: `tracker.go` — State machine

- States: `stateTracking`, `stateIdle`
- `Run(ctx)`: ticker at `poll_interval`, each tick:
  - Read idle time + active window
  - TRACKING → idle ≥ threshold: finalize record (end = now - idle), go IDLE
  - TRACKING → window changed: finalize, start new record
  - IDLE → idle < threshold: start new record, go TRACKING
- `finalize(endedAt)`: calculate duration, discard if ≤ 0s, enqueue to reporter
- `startNew(info)`: set current record with app/title/startedAt
- On ctx.Done: finalize current record if any

### Step 6: `cmd/trackkrd/main.go` — Entry point

- Config loading, zerolog setup
- Create XWindowDetector, IdleDetectorOrNop, Reporter, Tracker
- `signal.NotifyContext` for SIGINT/SIGTERM
- Reporter.Run in background goroutine, Tracker.Run blocking
- On shutdown: Reporter.Shutdown, wait for goroutines

### Step 7: `deploy/trackkrd.service` + `mise.toml`

- systemd user unit with `DISPLAY=:0`, restart on failure
- mise tasks: `build-daemon`, `run-daemon`

## Key Design Decisions

- **xprintidle missing**: graceful degradation via `NopIdleDetector`, not a hard failure
- **Testability**: interfaces for WindowDetector, IdleDetector, HTTPPoster — all mockable
- **Atomic file writes**: pending.json written via temp file + rename to prevent corruption
- **No Wayland support** (Phase 6) — xdotool fails clearly on Wayland with an error message

## Verification

1. `go build ./cmd/trackkrd` compiles
2. `mise test` + `mise test-race` pass with ≥50% coverage
3. `mise lint` clean
4. Manual: run daemon with server up, switch windows, verify records in DB
