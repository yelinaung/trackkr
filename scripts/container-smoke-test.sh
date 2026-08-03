#!/usr/bin/env bash
set -euo pipefail

smoke_id="trackkr-smoke-$$"
network_name="${smoke_id}-network"
database_name="${smoke_id}-database"
server_name="${smoke_id}-server"
image_name="trackkr-server:smoke"
postgres_image="postgres:18-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15"
alpine_image="alpine@sha256:79ff19e9084a00eece421b2523fb93e22d730e2c0e525905de047e848e56d95f"
database_password="$(openssl rand -hex 24)"
database_url="postgres://trackkr"
database_url+=":${database_password}@db:5432/trackkr"

cleanup() {
  docker rm -f "${server_name}" "${database_name}" >/dev/null 2>&1 || true
  docker network rm "${network_name}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker build --tag "${image_name}" \
  --build-arg VERSION=smoke \
  --build-arg COMMIT=smoke \
  --build-arg BUILD_DATE=2026-08-02T00:00:00Z \
  .
if [ "$(docker run --rm "${image_name}" version)" != "version=smoke commit=smoke build_date=2026-08-02T00:00:00Z" ]; then
  echo "image version metadata did not reach the binary" >&2
  exit 1
fi
docker network create "${network_name}" >/dev/null
docker run -d --name "${database_name}" --network "${network_name}" --network-alias db \
  --env POSTGRES_DB=trackkr \
  --env POSTGRES_USER=trackkr \
  --env POSTGRES_PASSWORD="${database_password}" \
  "${postgres_image}" >/dev/null

for _ in $(seq 1 30); do
  if docker exec "${database_name}" psql --username trackkr --dbname trackkr --command "SELECT 1" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
if ! docker exec "${database_name}" psql --username trackkr --dbname trackkr --command "SELECT 1" >/dev/null 2>&1; then
  docker logs "${database_name}"
  exit 1
fi

docker run --rm --network "${network_name}" \
  --env DATABASE_URL="${database_url}" \
  "${image_name}" migrate
docker run --rm --network "${network_name}" \
  --env DATABASE_URL="${database_url}" \
  "${image_name}" migration-status

docker run -d --name "${server_name}" --network "${network_name}" --network-alias trackkr \
  --env DATABASE_URL="${database_url}" \
  --env TRACKKR_SESSION_SECRET=container-smoke-session-secret-0123456789 \
  --env PORT=8080 \
  "${image_name}" serve >/dev/null

for _ in $(seq 1 30); do
  if docker run --rm --network "${network_name}" "${alpine_image}" \
    wget -q -T 1 -O /dev/null http://trackkr:8080/readyz; then
    exit 0
  fi
  if ! docker inspect "${server_name}" --format '{{.State.Running}}' | grep -q true; then
    docker logs "${server_name}"
    exit 1
  fi
  sleep 1
done

docker logs "${server_name}"
exit 1
