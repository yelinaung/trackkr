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

export GOCACHE="$PWD/.cache/go-build"
export GOMODCACHE="$PWD/.cache/go-mod"
export TRACKKR_DB_PASSWORD="trackkr"
# Fixed rather than random so restarting the server does not invalidate
# the session cookie in your browser. It is a dev value and is not a
# secret; production reads this from the environment.
export TRACKKR_SESSION_SECRET="dev-session-secret-0123456789abcdef"

mkdir -p "$DEV_DIR"

say() { printf '\033[36m==>\033[0m %s\n' "$1"; }

# A stale server from a previous run holds the port and the new one
# exits, leaving the old binary serving while you read the new logs --
# which is exactly as confusing as it sounds. Fail here instead.
for port in "$DEV_PORT" 7600; do
  if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "port $port is already in use; stop the previous run first:" >&2
    lsof -nP -iTCP:"$port" -sTCP:LISTEN | tail -n +2 >&2
    exit 1
  fi
done

say "starting postgres"
docker compose up -d db >/dev/null

say "waiting for postgres"
for _ in $(seq 1 30); do
  if docker compose exec -T db pg_isready -U trackkr -q 2>/dev/null; then
    break
  fi
  sleep 1
done

# Written before the server starts so it can read the port; migrations
# run automatically on server start.
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

EXT_TOKEN="$(go run ./cmd/trackkrd -print-extension-token)"

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

cleanup() {
  say "stopping"
  # go run execs a child, so killing the process group is what actually
  # stops the server rather than orphaning it on the port.
  kill -- "-$$" 2>/dev/null || true
  kill "$SERVER_PID" 2>/dev/null || true
  wait "$SERVER_PID" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

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
go run ./cmd/trackkrd -config "$CONFIG"
