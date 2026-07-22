#!/usr/bin/env bash
# Load / performance run (testing-plan Part 8 / Goal T10). Brings up a single-node
# cloud stack with pprof enabled (docker-compose.load.yml), then:
#   Phase A  submission throughput  -- k6 ramps the guest submission arrival rate,
#            records p95 latency + error rate (finds the knee).
#   Phase B  SSE fan-out boundedness -- k6 holds N concurrent /jobs/:id/stream
#            subscribers on one queued job while this script snapshots the cloud's
#            goroutine count via pprof (idle -> peak -> after release).
#   Phase C  dispatch backlog drain -- with the printer still offline, k6 queues a
#            burst of jobs onto the jobs:pending Stream (Phase A went out via
#            immediate dispatch and never touched it), then the printer returns and
#            this script times the consumer group draining the backlog to zero.
# Writes scripts/load/report/summary.json and checks it against the committed
# baseline (scripts/load/baseline.json); a breach fails the run.
#
# Env knobs:
#   KEEP_STACK=1   leave the stack up after the run
#   NO_BUILD=1     skip the image rebuild
#   SUBSCRIBERS=N  fan-out subscriber count (default 150)
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
ROOT="$(pwd)"

COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.load.yml)
CLOUD_URL="http://localhost:8080"
PPROF_URL="http://localhost:6060"
REPORT_DIR="scripts/load/report"
SUBSCRIBERS="${SUBSCRIBERS:-150}"
HOLD_SECONDS=25
MAILBOX_ID="${DEV_MAILBOX_ID:-00000000-0000-0000-0000-000000000001}"

BURST="${BURST:-60}" # Phase C: jobs queued while the printer is offline

# The k6 container writes its summary into the bind-mounted report dir; run it as
# the invoking user so the write is permitted and the file is host-user-owned
# (see the `user:` mapping on the k6 service in docker-compose.load.yml).
export LOAD_UID="$(id -u)" LOAD_GID="$(id -g)"

PG_USER="$(grep '^POSTGRES_USER=' .env | cut -d= -f2-)"
PG_PASS="$(grep '^POSTGRES_PASSWORD=' .env | cut -d= -f2-)"
PG_DB="$(grep '^POSTGRES_DB=' .env | cut -d= -f2-)"

# Count jobs still waiting on dispatch. This is the application-level truth for
# "the consumer group drained the backlog" -- clearer than XLEN, which never
# shrinks (Stream entries survive XACK). Returns -1 (never equal to 0) if the
# query fails, so a broken probe can't be mistaken for "drained".
queued_jobs() {
  local out
  out="$("${COMPOSE[@]}" exec -T -e PGPASSWORD="$PG_PASS" postgres \
    psql -tAq -U "$PG_USER" -d "$PG_DB" \
    -c "select count(*) from jobs where status in ('submitted','queued')" 2>/dev/null || true)"
  out="$(printf '%s' "$out" | tr -cd '0-9')"
  printf '%s' "${out:--1}"
}

mkdir -p "$REPORT_DIR"

cleanup() {
  if [ "${KEEP_STACK:-}" = "1" ]; then echo "==> KEEP_STACK=1: leaving stack up"; return; fi
  echo "==> Tearing down stack"
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Parse "goroutine profile: total N" off the pprof endpoint. Deliberately does
# NOT pipe into head/grep: this script runs under `set -o pipefail`, and closing
# the pipe early makes curl die of SIGPIPE, so the pipeline reports non-zero even
# when the match succeeded -- the `|| echo 0` fallback then appended a second
# line and every goroutine count came out as "N\n0", breaking the integer
# comparisons below. Capturing the body first and matching in-shell avoids the
# pipe entirely.
pprof_goroutines() {
  local body
  body="$(curl -s --max-time 5 "${PPROF_URL}/debug/pprof/goroutine?debug=1" || true)"
  if [[ "$body" =~ total[[:space:]]+([0-9]+) ]]; then
    printf '%s' "${BASH_REMATCH[1]}"
  else
    printf '0'
  fi
}

echo "==> Bootstrapping secrets (idempotent)"
bash scripts/e2e/bootstrap.sh >/dev/null

echo "==> Bringing up a clean single-node load stack (pprof on)"
"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
if [ "${NO_BUILD:-}" = "1" ]; then
  "${COMPOSE[@]}" up -d postgres redis minio cloud-server printer
else
  "${COMPOSE[@]}" up -d --build postgres redis minio cloud-server printer
fi

echo "==> Waiting for cloud-server (${CLOUD_URL}/healthz)"
for i in $(seq 1 60); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' "${CLOUD_URL}/healthz" || true)" = "200" ] && { echo "   ready after ${i}s"; break; }
  sleep 1
done

echo "==> Waiting for the printer liveness cache"
for i in $(seq 1 30); do
  "${COMPOSE[@]}" exec -T redis redis-cli GET "mailbox:${MAILBOX_ID}:state" | grep -q idle && { echo "   printer live after ${i}s"; break; }
  sleep 1
done

E2E_COMPOSE_FILES="-f docker-compose.yml -f docker-compose.load.yml" bash scripts/e2e/seed.sh

RECIPIENT="$(curl -s "${CLOUD_URL}/recipients?q=Testmann" | python3 -c 'import sys,json; print(json.load(sys.stdin)[0]["recipient_id"])')"
echo "==> Seeded recipient: ${RECIPIENT}"

IDLE_G="$(pprof_goroutines)"
echo "==> Idle goroutines: ${IDLE_G}"

echo "==> Phase A: submission throughput (k6 ramping arrival rate)"
rm -f "${REPORT_DIR}/submission.json"
"${COMPOSE[@]}" run --rm \
  -e BASE_URL="http://cloud-server:8080" -e RECIPIENT_ID="$RECIPIENT" \
  k6 run /scripts/submission.js

# k6 exits 0 even when handleSummary fails to write its export (e.g. a uid
# mismatch on the bind-mounted report dir), which otherwise only surfaces much
# later as a confusing missing-file traceback while building the summary. Fail
# here, where the cause is obvious.
if [ ! -s "${REPORT_DIR}/submission.json" ]; then
  echo "!! Phase A produced no ${REPORT_DIR}/submission.json -- k6's handleSummary export failed" >&2
  echo "   (check the k6 'failed to handle the end-of-test summary' line above; usually a" >&2
  echo "    permission mismatch on the report bind-mount -- see the k6 user: mapping in docker-compose.load.yml)" >&2
  exit 1
fi

echo "==> Phase B: SSE fan-out boundedness (${SUBSCRIBERS} subscribers)"
# Stop the printer so the hold job stays non-terminal ('queued') for the hold.
"${COMPOSE[@]}" stop printer >/dev/null
sleep 4 # let the hub drop the printer's dispatch subscriber

"${COMPOSE[@]}" run --rm \
  -e BASE_URL="http://cloud-server:8080" -e RECIPIENT_ID="$RECIPIENT" \
  -e SUBSCRIBERS="$SUBSCRIBERS" -e HOLD_SECONDS="$HOLD_SECONDS" \
  k6 run /scripts/sse_fanout.js > "${REPORT_DIR}/fanout.log" 2>&1 &
FANOUT_PID=$!

# Poll goroutines while the subscribers are held; capture the peak.
PEAK_G="$IDLE_G"
for i in $(seq 1 $((HOLD_SECONDS - 3))); do
  g="$(pprof_goroutines)"
  [ "$g" -gt "$PEAK_G" ] && PEAK_G="$g"
  sleep 1
done
wait "$FANOUT_PID" || { echo "!! fan-out k6 failed:"; cat "${REPORT_DIR}/fanout.log"; exit 1; }
sleep 4 # let closed connections' goroutines wind down
RESIDUAL_G="$(pprof_goroutines)"
echo "==> Goroutines: idle=${IDLE_G} peak=${PEAK_G} residual=${RESIDUAL_G}"

# --- Phase C: dispatch backlog drain (consumer-group lag bounded).
# The printer is still stopped, so every job submitted now is forced down the
# QUEUED path onto jobs:pending (Phase A went out via immediate dispatch and
# never touched the Stream). Then bring the printer back and time how long the
# consumer group takes to drain the backlog to zero.
echo "==> Phase C: dispatch backlog (${BURST} jobs queued with the printer offline)"
"${COMPOSE[@]}" run --rm \
  -e BASE_URL="http://cloud-server:8080" -e RECIPIENT_ID="$RECIPIENT" \
  -e BURST="$BURST" \
  k6 run /scripts/dispatch_burst.js > "${REPORT_DIR}/dispatch.log" 2>&1 \
  || { echo "!! dispatch burst k6 failed:"; cat "${REPORT_DIR}/dispatch.log"; exit 1; }

BACKLOG_BEFORE="$(queued_jobs)"
STREAM_LEN="$("${COMPOSE[@]}" exec -T redis redis-cli XLEN jobs:pending | tr -d '[:space:]')"
echo "   backlog before drain: ${BACKLOG_BEFORE} queued job(s), jobs:pending XLEN=${STREAM_LEN}"
if [ "$BACKLOG_BEFORE" -eq 0 ]; then
  echo "!! Phase C queued nothing -- the printer still looked dispatchable, so this" >&2
  echo "   run did not exercise the Stream path at all. Check that the printer was stopped." >&2
  exit 1
fi

echo "==> Restarting the printer; timing the backlog drain"
"${COMPOSE[@]}" start printer >/dev/null
DRAIN_START="$(date +%s)"
DRAIN_SECONDS=-1
for i in $(seq 1 120); do
  if [ "$(queued_jobs)" -eq 0 ]; then
    DRAIN_SECONDS=$(( $(date +%s) - DRAIN_START ))
    echo "   backlog drained in ${DRAIN_SECONDS}s"
    break
  fi
  sleep 1
done
if [ "$DRAIN_SECONDS" -lt 0 ]; then
  echo "!! backlog did NOT drain within 120s (still $(queued_jobs) queued) -- the consumer" >&2
  echo "   group is not keeping up; see 'dispatch:' lines in the cloud-server logs." >&2
  "${COMPOSE[@]}" logs --tail=40 cloud-server >&2 || true
  exit 1
fi

echo "==> Building summary.json"
python3 - "$REPORT_DIR/submission.json" "$REPORT_DIR/summary.json" \
  "$SUBSCRIBERS" "$IDLE_G" "$PEAK_G" "$RESIDUAL_G" "$STREAM_LEN" \
  "$BACKLOG_BEFORE" "$DRAIN_SECONDS" <<'PY'
import json, sys
src, dst, subs, idle, peak, residual, stream_len, backlog, drain = sys.argv[1:10]
with open(src) as f:
    m = json.load(f)["metrics"]
def val(name, key, default=0.0):
    return m.get(name, {}).get("values", {}).get(key, default)
summary = {
    "submission": {
        "p95_ms": round(val("submit_duration", "p(95)"), 2),
        "avg_ms": round(val("submit_duration", "avg"), 2),
        "error_rate": round(val("submit_failed", "rate"), 4),
        "rps": round(val("iterations", "rate"), 2),
        "count": int(val("iterations", "count")),
    },
    "sse_fanout": {
        "subscribers": int(subs),
        "idle_goroutines": int(idle),
        "peak_goroutines": int(peak),
        "residual_goroutines": int(residual),
    },
    "dispatch": {
        "queued_backlog": int(backlog),
        "drain_seconds": int(drain),
        "stream_len": int(stream_len),
    },
}
with open(dst, "w") as f:
    json.dump(summary, f, indent=2)
print(json.dumps(summary, indent=2))
PY

echo "==> Checking against baseline"
python3 scripts/load/check-baseline.py "$REPORT_DIR/summary.json" scripts/load/baseline.json
