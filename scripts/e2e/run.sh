#!/usr/bin/env bash
# Orchestrates the portal browser E2E (testing-plan Part 4b / Goal T7):
# bring up a CLEAN compose stack, seed the fixture, run Playwright headless,
# then tear down. A fresh stack per run is what keeps the suite deterministic
# (the dev printer's slot occupancy only ever increments, so stale state would
# eventually fill the slot and block dispatch).
#
# Env knobs:
#   KEEP_STACK=1   leave the stack running after the tests (for debugging)
#   NO_BUILD=1     skip the image rebuild (faster re-runs when code is unchanged)
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.e2e.yml)
BASE_URL="http://localhost:3000"

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

echo "==> Bringing up a clean stack"
"${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
if [ "${NO_BUILD:-}" = "1" ]; then
  "${COMPOSE[@]}" up -d
else
  "${COMPOSE[@]}" up -d --build
fi

echo "==> Waiting for the API to serve (portal -> cloud -> Postgres)"
ready=""
for i in $(seq 1 60); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/api/recipients?q=zz" || true)"
  if [ "$code" = "200" ]; then ready="1"; echo "   ready after ${i}s"; break; fi
  sleep 1
done
if [ -z "$ready" ]; then
  echo "!! API did not become ready in time; recent cloud-server logs:" >&2
  "${COMPOSE[@]}" logs --tail=30 cloud-server >&2 || true
  exit 1
fi

# Wait for the printer to register and seed its liveness cache, so the first
# job dispatches immediately instead of queueing.
echo "==> Waiting for the printer liveness cache"
for i in $(seq 1 30); do
  if "${COMPOSE[@]}" exec -T redis redis-cli GET \
      "mailbox:${DEV_MAILBOX_ID:-00000000-0000-0000-0000-000000000001}:state" \
      | grep -q idle; then echo "   printer live after ${i}s"; break; fi
  sleep 1
done

bash scripts/e2e/seed.sh

echo "==> Ensuring Playwright + browser are installed"
cd services/portal
[ -d node_modules/@playwright/test ] || npm install
npx playwright install chromium >/dev/null

echo "==> Running Playwright"
PLAYWRIGHT_BASE_URL="$BASE_URL" npx playwright test --project=chromium
