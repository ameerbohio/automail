#!/usr/bin/env bash
# Orchestrates the resilience/chaos suite (testing-plan Part 7 / Goal T9): bring
# up a CLEAN two-node compose stack (docker-compose.full.yml, same topology as
# the Goal T8 full-system E2E), seed the fixture, then run the Go chaos driver
# (e2e/, -tags chaos), which kills Redis / Postgres / the socket-owning cloud
# node / the printer in turn and asserts every job still reaches a terminal
# state exactly once with no crash. Tears the stack down at the end.
#
# A fresh stack per run keeps it deterministic (the dev printer's slot occupancy
# only ever increments). Env knobs:
#   KEEP_STACK=1   leave the stack up after the run (debugging)
#   NO_BUILD=1     skip the image rebuild (faster re-runs when code is unchanged)
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
ROOT="$(pwd)"

COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.full.yml)
OWNER_URL="http://localhost:8080"     # cloud-server: holds the printer socket
NONOWNER_URL="http://localhost:8081"  # cloud-server-2: survivor node

cleanup() {
  if [ "${KEEP_STACK:-}" = "1" ]; then
    echo "==> KEEP_STACK=1: leaving stack up"
    return
  fi
  echo "==> Tearing down stack"
  "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> Bootstrapping secrets (idempotent)"
bash scripts/e2e/bootstrap.sh >/dev/null

echo "==> Bringing up a clean two-node stack"
"${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
if [ "${NO_BUILD:-}" = "1" ]; then
  "${COMPOSE[@]}" up -d
else
  "${COMPOSE[@]}" up -d --build
fi

# Both cloud nodes must serve before we start breaking things. /healthz is 200
# only when that node's Postgres AND Redis are reachable.
wait_ready() {
  local name="$1" url="$2" i code
  echo "==> Waiting for ${name} (${url}/healthz)"
  for i in $(seq 1 60); do
    code="$(curl -s -o /dev/null -w '%{http_code}' "${url}/healthz" || true)"
    if [ "$code" = "200" ]; then echo "   ${name} ready after ${i}s"; return 0; fi
    sleep 1
  done
  echo "!! ${name} did not become ready; recent logs:" >&2
  "${COMPOSE[@]}" logs --tail=30 >&2 || true
  exit 1
}
wait_ready "cloud-server (owner)" "$OWNER_URL"
wait_ready "cloud-server-2 (survivor)" "$NONOWNER_URL"

echo "==> Waiting for the printer liveness cache"
printer_live=""
for i in $(seq 1 30); do
  if "${COMPOSE[@]}" exec -T redis redis-cli GET \
      "mailbox:${DEV_MAILBOX_ID:-00000000-0000-0000-0000-000000000001}:state" \
      | grep -q idle; then printer_live="1"; echo "   printer live after ${i}s"; break; fi
  sleep 1
done
if [ -z "$printer_live" ]; then
  echo "!! printer never reported idle; recent printer logs:" >&2
  "${COMPOSE[@]}" logs --tail=30 printer >&2 || true
  exit 1
fi

# Seed against THIS stack's project (full compose files), not the browser-E2E pair.
E2E_COMPOSE_FILES="-f docker-compose.yml -f docker-compose.full.yml" bash scripts/e2e/seed.sh

# Postgres creds for the driver's exactly-once assertions (audit_events counts).
PG_USER="$(grep '^POSTGRES_USER=' .env | cut -d= -f2-)"
PG_DB="$(grep '^POSTGRES_DB=' .env | cut -d= -f2-)"
PG_PASSWORD="$(grep '^POSTGRES_PASSWORD=' .env | cut -d= -f2-)"

echo "==> Running the chaos driver"
cd e2e
E2E_OWNER_URL="$OWNER_URL" \
E2E_NONOWNER_URL="$NONOWNER_URL" \
E2E_REPO_ROOT="$ROOT" \
E2E_PG_USER="$PG_USER" \
E2E_PG_DB="$PG_DB" \
E2E_PG_PASSWORD="$PG_PASSWORD" \
  go test -tags chaos -count=1 -v -timeout 15m ./...
