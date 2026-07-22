#!/usr/bin/env bash
# Orchestrates the full-system E2E (testing-plan Part 5 / Goal T8): bring up a
# CLEAN TWO-NODE compose stack (docker-compose.full.yml), seed the fixture, run
# the standalone Go driver (e2e/) that pushes a real encrypted job all the way to
# "delivered" and asserts the printer /dev/shm wipe + the cross-node fan-in /
# fan-out, then tear down.
#
# A fresh stack per run keeps it deterministic: the dev printer's slot occupancy
# only ever increments, so stale state would eventually fill the slot and block
# dispatch. Env knobs:
#   KEEP_STACK=1   leave the stack up after the run (debugging)
#   NO_BUILD=1     skip the image rebuild (faster re-runs when code is unchanged)
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
ROOT="$(pwd)"

COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.full.yml)
OWNER_URL="http://localhost:8080"     # cloud-server: holds the printer socket
NONOWNER_URL="http://localhost:8081"  # cloud-server-2: never holds the socket

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

# Both cloud nodes must serve before we drive them. /healthz returns 200 only
# when that node's Postgres AND Redis are reachable (TestHealthz_Readiness).
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
wait_ready "cloud-server-2 (non-owner)" "$NONOWNER_URL"

# The printer must have registered its dial-out socket on the owner and seeded
# its liveness cache, so the first job dispatches immediately (status
# "dispatching") instead of queueing -- the fan-in assertion depends on it.
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

echo "==> Running the full-system Go driver"
cd e2e
E2E_OWNER_URL="$OWNER_URL" \
E2E_NONOWNER_URL="$NONOWNER_URL" \
E2E_REPO_ROOT="$ROOT" \
  go test -tags e2e -count=1 -v -timeout 5m ./...
