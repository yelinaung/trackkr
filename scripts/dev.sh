#!/usr/bin/env bash
#
# One command to get the whole stack running: database, server, a dev
# account, a device, a daemon config, and the daemon itself.
#
# Nothing here is meant to persist. The account and device are recreated
# on every run and the daemon config is written under .cache/, so this
# never touches ~/Library/Application Support or ~/.config. The point is
# to see the system work, not to configure a machine.
#
# Ctrl-C stops the server and the daemon together; the database keeps
# running, since starting Postgres is the slow part.

set -euo pipefail

DEV_USER="dev"
DEV_PASSWORD="dev-password-not-a-secret"
DEV_PORT="${TRACKKR_DEV_PORT:-8080}"
DEV_DIR="$PWD/.cache/trackkr-dev"
CONFIG="$DEV_DIR/config.toml"
LAUNCH_AGENT_LABEL="com.trackkr.daemon"
LAUNCH_AGENT_PLIST="$HOME/Library/LaunchAgents/$LAUNCH_AGENT_LABEL.plist"
LAUNCH_AGENT_PAUSED=0
SERVER_PID=""
DAEMON_PID=""

export GOCACHE="$PWD/.cache/go-build"
export GOMODCACHE="$PWD/.cache/go-mod"
export TRACKKR_DB_PASSWORD="trackkr"
# Fixed rather than random so restarting the server does not invalidate
# the session cookie in your browser. It is a dev value and is not a
# secret; production reads this from the environment.
export TRACKKR_SESSION_SECRET="dev-session-secret-0123456789abcdef"

mkdir -p "$DEV_DIR"

say() { printf '\033[36m==>\033[0m %s\n' "$1"; }

stop_pids() {
  local label="$1"
  shift
  local pids=("$@")
  [ "${#pids[@]}" -gt 0 ] || return 0

  say "stopping $label"
  kill -TERM "${pids[@]}" 2>/dev/null || true
  for _ in $(seq 1 30); do
    local running=()
    local pid
    for pid in "${pids[@]}"; do
      if kill -0 "$pid" 2>/dev/null; then
        running+=("$pid")
      fi
    done
    [ "${#running[@]}" -gt 0 ] || return 0
    pids=("${running[@]}")
    sleep 0.1
  done

  say "forcing $label to stop"
  kill -KILL "${pids[@]}" 2>/dev/null || true
}

stop_port_listener() {
  local port="$1"
  local pids=()
  local pid command
  while IFS= read -r pid; do
    [ -n "$pid" ] || continue
    command="$(ps -p "$pid" -o command= 2>/dev/null || true)"
    case "$command" in
      *trackkr-backend* | *trackkrd* | *go-build*/exe/server*)
        pids+=("$pid")
        ;;
      *)
        echo "port $port is used by a process that does not look like trackkr:" >&2
        lsof -nP -iTCP:"$port" -sTCP:LISTEN | tail -n +2 >&2
        exit 1
        ;;
    esac
  done < <(lsof -nP -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | sort -u)
  stop_pids "stale listener on port $port" "${pids[@]}"

  if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "could not stop the process listening on port $port:" >&2
    lsof -nP -iTCP:"$port" -sTCP:LISTEN | tail -n +2 >&2
    exit 1
  fi
}

# The installed macOS app runs trackkrd under launchd with KeepAlive set, on
# the same extension port this script uses. Killing that process is futile:
# launchd restarts it within a second, and the dev daemon -- which starts a
# good twenty seconds later, after docker and three "go run" compiles -- then
# fails to bind. Unload the job for the duration and restore it on the way out.
pause_launch_agent() {
  command -v launchctl >/dev/null 2>&1 || return 0
  [ -f "$LAUNCH_AGENT_PLIST" ] || return 0
  launchctl print "gui/$UID/$LAUNCH_AGENT_LABEL" >/dev/null 2>&1 || return 0

  say "unloading the installed $LAUNCH_AGENT_LABEL agent"
  if ! launchctl bootout "gui/$UID/$LAUNCH_AGENT_LABEL" 2>/dev/null; then
    echo "could not unload $LAUNCH_AGENT_LABEL; stop it by hand with" >&2
    echo "  launchctl bootout gui/\$UID/$LAUNCH_AGENT_LABEL" >&2
    exit 1
  fi
  LAUNCH_AGENT_PAUSED=1
}

resume_launch_agent() {
  [ "$LAUNCH_AGENT_PAUSED" -eq 1 ] || return 0
  LAUNCH_AGENT_PAUSED=0
  say "restoring the installed $LAUNCH_AGENT_LABEL agent"
  launchctl bootstrap "gui/$UID" "$LAUNCH_AGENT_PLIST" 2>/dev/null || true
}

cleanup() {
  # Disarm first. Anything that signals this shell -- directly or via
  # its process group -- would otherwise re-enter this function through
  # the TERM trap and never reach the kills below.
  trap - EXIT INT TERM

  local pids=()
  for pid in "$DAEMON_PID" "$SERVER_PID"; do
    [ -n "$pid" ] || continue
    # Include any compiled child that "go run" has not execed into.
    while IFS= read -r child; do
      [ -n "$child" ] && pids+=("$child")
    done < <(pgrep -P "$pid" 2>/dev/null || true)
    pids+=("$pid")
  done
  if [ "${#pids[@]}" -gt 0 ]; then
    say "stopping"
  fi
  stop_pids "server and daemon" "${pids[@]}"
  for pid in "$DAEMON_PID" "$SERVER_PID"; do
    [ -n "$pid" ] && wait "$pid" 2>/dev/null || true
  done
  resume_launch_agent
}

# Armed before the agent is unloaded, so an early failure -- an unreachable
# database, say -- still puts the agent back.
trap cleanup EXIT INT TERM
pause_launch_agent

# Reclaim both dev listeners before doing setup. This handles an interrupted
# previous run even when "go run" left its compiled child orphaned.
stop_port_listener "$DEV_PORT"
stop_port_listener 7600

say "starting postgres"
docker compose up -d db >/dev/null

say "waiting for postgres"
for _ in $(seq 1 30); do
  if docker compose exec -T db pg_isready -U trackkr -q 2>/dev/null; then
    break
  fi
  sleep 1
done

# Written before the migration and server commands start so both use the
# same local database and HTTP configuration.
cat > "$DEV_DIR/server.toml" <<EOF
[server]
host = "127.0.0.1"
port = $DEV_PORT
timezone = "Local"
# Plain HTTP locally, so Secure cookies would never be sent back.
secure_cookies = false

[database]
host = "localhost"
port = 5455
name = "trackkr"
user = "trackkr"
sslmode = "disable"

[auth]
allow_registration = false
EOF
export TRACKKR_CONFIG="$DEV_DIR/server.toml"

say "running database migrations"
go run ./cmd/server migrate

# The account survives between runs; a duplicate is not an error here.
say "ensuring the $DEV_USER account exists"
go run ./cmd/server create-user "$DEV_USER" "$DEV_PASSWORD" >/dev/null 2>&1 ||
  say "account already exists, reusing it"

say "creating a device"
API_KEY="$(go run ./cmd/server create-device "$DEV_USER" "dev-$(hostname -s)" 2>/dev/null | tail -1)"
if [ -z "$API_KEY" ]; then
  echo "could not create a device; is the database reachable?" >&2
  exit 1
fi

# Persist the token across runs. Regenerating it every time means the
# extension silently stops working after any restart, and the popup can
# only report "token rejected" -- which is accurate but tedious.
TOKEN_FILE="$DEV_DIR/extension-token"
if [ ! -s "$TOKEN_FILE" ]; then
  go run ./cmd/trackkrd -print-extension-token > "$TOKEN_FILE"
fi
EXT_TOKEN="$(cat "$TOKEN_FILE")"

cat > "$CONFIG" <<EOF
# Written by scripts/dev.sh on each run. Not the file the daemon uses
# by default -- that lives under your user config directory.
server_url = "http://127.0.0.1:$DEV_PORT"
api_key = "$API_KEY"
poll_interval = "3s"
data_dir = "$DEV_DIR"

# Enabled unconditionally: on macOS there is no window detector yet, so
# without this the daemon has nothing to track and exits.
extension_enabled = true
extension_addr = "127.0.0.1:7600"
extension_token = "$EXT_TOKEN"
EOF

say "starting the server on http://127.0.0.1:$DEV_PORT"
go run ./cmd/server &
SERVER_PID=$!

for _ in $(seq 1 30); do
  if curl -sS -o /dev/null -m 1 "http://127.0.0.1:$DEV_PORT/login" 2>/dev/null; then
    break
  fi
  sleep 1
done

cat <<EOF

  dashboard   http://127.0.0.1:$DEV_PORT/login
  sign in     $DEV_USER / $DEV_PASSWORD
  daemon cfg  $CONFIG
  ext token   $EXT_TOKEN

  Paste the token into the extension's options page, and load the
  extension from ./extension via about:debugging.

EOF

say "starting the daemon (Ctrl-C stops everything)"
# Backgrounded and waited on rather than run in the foreground: bash
# defers trap handlers until a foreground child exits, so a signal sent
# to this script would otherwise be ignored until the daemon happened
# to stop on its own -- leaving both processes holding their ports.
go run ./cmd/trackkrd -config "$CONFIG" &
DAEMON_PID=$!
wait "$DAEMON_PID"
