#!/usr/bin/env bash
# Graceful-shutdown acceptance (Goal K0 / plans/16-kubernetes.md §4.1-§4.2).
#
# Two phases, because the two properties need different stacks:
#
#   Phase A -- consumer-group lifecycle. Base compose scaled to 3 cloud-servers
#     (no host port needed; the assertions are Redis-side). Cycle up -> stop ->
#     up and require the `dispatchers` consumer count to equal the number of
#     LIVE nodes each time. Before Goal K0 the count only ever grew: 4
#     consumers for 3 live nodes was the measurement that started this goal.
#     Containers are stopped with `docker compose stop` (SIGTERM) and their exit
#     codes checked -- a SIGKILLed drain (137) proves nothing, and an unhandled
#     SIGTERM exits 143.
#
#   Phase B -- in-flight request drain. The two-node full stack, a real
#     encrypted job, its SSE status stream held open, then SIGTERM. The Go
#     driver (e2e/, tag `shutdown`) asserts the stream ends with the server's
#     own `: draining` notice instead of being severed.
#
# Env knobs:
#   KEEP_STACK=1   leave the stack up after the run (debugging)
#   NO_BUILD=1     skip the image rebuild (faster re-runs when code is unchanged)
#   PHASE=a|b      run only one phase
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
ROOT="$(pwd)"

BASE=(docker compose -f docker-compose.yml)
FULL=(docker compose -f docker-compose.yml -f infra/compose/full.yml)
PHASE="${PHASE:-ab}"

cleanup() {
  if [ "${KEEP_STACK:-}" = "1" ]; then
    echo "==> KEEP_STACK=1: leaving stack up"
    return
  fi
  echo "==> Tearing down"
  "${FULL[@]}" down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> Bootstrapping secrets (idempotent)"
bash scripts/e2e/bootstrap.sh >/dev/null

# consumer_count prints how many consumers are registered in the dispatchers
# group. redis-cli in non-TTY mode prints one field per line, so counting the
# "name" keys is a stable way to read XINFO CONSUMERS without parsing RESP.
# A missing stream/group (nothing has started yet) counts as 0.
consumer_count() {
  "${BASE[@]}" exec -T redis redis-cli XINFO CONSUMERS jobs:pending dispatchers 2>/dev/null \
    | grep -c '^name$' || true
}

# wait_consumers polls until the count matches, then prints it. Startup
# registers a consumer only once a node issues its first XREADGROUP (the drain
# Run does immediately), so a second or two of lag is normal.
wait_consumers() {
  local want="$1" i got
  for i in $(seq 1 60); do
    got="$(consumer_count)"
    if [ "$got" = "$want" ]; then echo "   consumers = $got after ${i}s"; return 0; fi
    sleep 1
  done
  echo "!! consumer count = ${got}, want ${want}" >&2
  "${BASE[@]}" exec -T redis redis-cli XINFO CONSUMERS jobs:pending dispatchers >&2 || true
  return 1
}

phase_a() {
  echo
  echo "=== Phase A: consumer group tracks the live nodes ==="
  "${FULL[@]}" down -v >/dev/null 2>&1 || true

  echo "==> Bringing up 3 cloud-server replicas"
  if [ "${NO_BUILD:-}" = "1" ]; then
    "${BASE[@]}" up -d --scale cloud-server=3 cloud-server
  else
    "${BASE[@]}" up -d --build --scale cloud-server=3 cloud-server
  fi

  echo "==> Expect 3 consumers for 3 live nodes"
  wait_consumers 3

  echo "==> SIGTERM all 3 (docker compose stop, never kill -9)"
  "${BASE[@]}" stop cloud-server

  # An unhandled SIGTERM exits 143; a grace-period SIGKILL exits 137. Only a
  # process that handled the signal and returned from main exits 0.
  local cid code bad=0
  for cid in $("${BASE[@]}" ps -aq cloud-server); do
    code="$(docker inspect -f '{{.State.ExitCode}}' "$cid")"
    echo "   $(docker inspect -f '{{.Name}}' "$cid") exited $code"
    if [ "$code" != "0" ]; then bad=1; fi
  done
  if [ "$bad" = "1" ]; then
    echo "!! a cloud-server did not exit cleanly (143 = unhandled SIGTERM, 137 = SIGKILL after the grace period)" >&2
    "${BASE[@]}" logs --tail=40 cloud-server >&2 || true
    return 1
  fi

  echo "==> Expect the drain to have logged its sequence"
  if ! "${BASE[@]}" logs cloud-server 2>&1 | grep -q "shutdown complete"; then
    echo "!! no 'shutdown complete' in the logs" >&2
    "${BASE[@]}" logs --tail=40 cloud-server >&2 || true
    return 1
  fi
  "${BASE[@]}" logs cloud-server 2>&1 | grep -E "shutdown: |draining \(grace" | tail -12

  echo "==> Expect 0 consumers for 0 live nodes"
  wait_consumers 0

  echo "==> Bringing the 3 replicas back"
  "${BASE[@]}" up -d --scale cloud-server=3 cloud-server
  echo "==> Expect 3 consumers again (not 6)"
  wait_consumers 3

  echo "✔ Phase A: consumer count tracked live nodes across up -> stop -> up"
  "${BASE[@]}" down -v >/dev/null 2>&1 || true
}

phase_b() {
  echo
  echo "=== Phase B: an open SSE stream is drained, not severed ==="
  "${FULL[@]}" down -v >/dev/null 2>&1 || true

  echo "==> Bringing up the two-node stack"
  if [ "${NO_BUILD:-}" = "1" ]; then
    "${FULL[@]}" up -d
  else
    "${FULL[@]}" up -d --build
  fi

  local i code
  for i in $(seq 1 60); do
    code="$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8080/healthz || true)"
    if [ "$code" = "200" ]; then echo "   cloud-server ready after ${i}s"; break; fi
    sleep 1
  done
  if [ "$code" != "200" ]; then
    echo "!! cloud-server never became ready" >&2
    "${FULL[@]}" logs --tail=30 >&2 || true
    return 1
  fi

  # /livez must answer without touching Postgres or Redis (§4.4). Asserted here
  # too, since the unit test cannot prove the route is actually registered on
  # the public mux of the real image.
  code="$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8080/livez || true)"
  if [ "$code" != "200" ]; then
    echo "!! GET /livez returned ${code}, want 200" >&2
    return 1
  fi
  echo "   /livez serving (liveness split from readiness)"

  E2E_COMPOSE_FILES="-f docker-compose.yml -f infra/compose/full.yml" bash scripts/e2e/seed.sh

  echo "==> Running the shutdown driver"
  ( cd tests/system && \
    E2E_OWNER_URL="http://localhost:8080" \
    E2E_REPO_ROOT="$ROOT" \
    E2E_COMPOSE_OVERRIDE="infra/compose/full.yml" \
      go test -tags shutdown -count=1 -v -timeout 5m ./... )

  echo "✔ Phase B: the draining server closed the stream itself"
}

case "$PHASE" in
  a) phase_a ;;
  b) phase_b ;;
  *) phase_a; phase_b ;;
esac

echo
echo "✔ Graceful shutdown acceptance passed"
