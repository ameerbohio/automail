#!/usr/bin/env bash
# Deployment-parity smoke (Goal T12) -- the closing loop on "when the owner
# deploys to Proxmox it works on the first try".
#
# Unlike scripts/e2e/{run,full,chaos}.sh, which drive override stacks that publish
# services on host ports and bypass Traefik, this brings up the PRODUCTION PROFILE
# (docker-compose.yml as-is, plus the two documented deviations in
# docker-compose.deploy-smoke.yml) and drives it only through the HTTPS edge.
#
# Phase 0 is a preflight that checks the same prerequisites docs/deploy-checklist.md
# lists, so the checklist is executable rather than aspirational: every failure
# below prints the exact remediation command from that document.
#
# !! DESTRUCTIVE !! This drops the stack's volumes (`down -v`) before and after the
# run, and seeds non-production fixture rows -- including an admin account whose
# password is published in this repo (scripts/e2e/seed.sh). It is a pre-flight for
# a host, not a health check for a live one. Run it BEFORE provisioning real data,
# or on a throwaway host. It refuses to run if it finds a non-empty Postgres volume
# unless ALLOW_DESTRUCTIVE=1 says you meant it.
#
# Env knobs:
#   KEEP_STACK=1        leave the stack up after the run (debugging)
#   NO_BUILD=1          skip the image rebuild (faster re-runs when code is unchanged)
#   TRAEFIK_HTTPS_PORT  host port for the edge (default 8443 here; 443 on a real host)
#   ALLOW_DESTRUCTIVE=1 proceed even though an existing data volume will be destroyed
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
ROOT="$(pwd)"

# On a real deploy host the edge listens on 80/443 and the browser reaches it by
# name. On WSL2, Windows holds both, so publish elsewhere and point the driver's
# host-mapping at it. The base compose reads these, so nothing about the routed
# hostnames, SNI, or the certificate changes -- only where the socket lives.
export TRAEFIK_HTTP_PORT="${TRAEFIK_HTTP_PORT:-8080}"
export TRAEFIK_HTTPS_PORT="${TRAEFIK_HTTPS_PORT:-8443}"

COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.deploy-smoke.yml)

cleanup() {
  if [ "${KEEP_STACK:-}" = "1" ]; then
    echo "==> KEEP_STACK=1: leaving the stack up (edge on https://localhost:${TRAEFIK_HTTPS_PORT})"
    return
  fi
  echo "==> Tearing down"
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
}
fail() { echo "!! $1" >&2; echo "   fix: $2" >&2; exit 1; }

echo "==> [0/5] Preflight (docs/deploy-checklist.md prerequisites)"

# Secrets and keys. bootstrap.sh generates local non-production values; a real
# deploy runs the infra/certs/gen*.sh scripts by hand with real passphrases.
bash scripts/e2e/bootstrap.sh >/dev/null

[ -f .env ] || fail "no .env" "cp .env.example .env, then fill in every changeme* value"
for f in infra/certs/ca-cert.pem infra/certs/cloud-server-cert.pem infra/certs/printer-cert.pem; do
  [ -f "$f" ] || fail "missing $f" "./infra/certs/gen.sh"
done
[ -f infra/certs/jwt-private.pem ]     || fail "missing JWT keypair" "./infra/certs/gen-jwt-keys.sh"
[ -f infra/certs/printer-private.pem ] || fail "missing printer document keypair" \
  "PRINTER_KEY_PASSPHRASE=... ./infra/certs/gen-printer-keys.sh"
[ -f infra/traefik/edge-cert.pem ]     || fail "missing edge TLS certificate" "./infra/certs/gen-edge-certs.sh"

# The edge cert must cover every routed hostname or sniStrict rejects the
# handshake before HTTP happens. Checked here as well as in the Go suite so a
# stale cert (generated before blob.automail.local was routed) is reported as a
# prerequisite problem, not as a mysterious upload failure 90 seconds later.
sans="$(openssl x509 -in infra/traefik/edge-cert.pem -noout -ext subjectAltName 2>/dev/null || true)"
for host in automail.local api.automail.local blob.automail.local; do
  grep -q "DNS:${host}\b" <<<"$sans" \
    || fail "edge cert has no SAN for ${host} (has: ${sans//$'\n'/ })" \
            "rm infra/traefik/edge-*.pem && ./infra/certs/gen-edge-certs.sh"
done

# `docker compose config` resolving is the cheapest possible catch for an
# unset-variable or bad-override mistake, before any image is built.
"${COMPOSE[@]}" config >/dev/null || fail "compose config does not resolve" "check .env against .env.example"

# Refuse to destroy data the operator provisioned. `down -v` below would take the
# mailbox/slot/resident rows and every stored blob with it, and seed.sh would then
# write fixture rows over the top.
if docker volume inspect "$(basename "$ROOT")_pg_data" >/dev/null 2>&1 \
   && [ "${ALLOW_DESTRUCTIVE:-}" != "1" ]; then
  fail "an existing Postgres volume was found -- this script runs \`down -v\` and would destroy it" \
       "run this BEFORE provisioning real data, or re-run with ALLOW_DESTRUCTIVE=1 if the data is disposable"
fi

# The mailbox this stack's printer registers as. Compose reads it from .env, so
# read it the same way -- taking it only from the shell would poll the wrong
# mailbox key on any host that set a real id (docs/deploy-checklist.md §5).
DEV_MAILBOX_ID="${DEV_MAILBOX_ID:-$(sed -n 's/^DEV_MAILBOX_ID=//p' .env | tail -1)}"
DEV_MAILBOX_ID="${DEV_MAILBOX_ID:-00000000-0000-0000-0000-000000000001}"
export DEV_MAILBOX_ID
echo "   preflight OK (secrets, edge cert SANs, compose config, mailbox ${DEV_MAILBOX_ID})"

# Armed only now: before this point nothing has been started, so a preflight
# failure has nothing to tear down and should not claim otherwise.
trap cleanup EXIT

echo "==> [1/5] Bringing up a clean production-profile stack"
"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
if [ "${NO_BUILD:-}" = "1" ]; then
  "${COMPOSE[@]}" up -d
else
  "${COMPOSE[@]}" up -d --build
fi

# Readiness is checked THROUGH the edge on the routed hostname, not on a direct
# port: that is the path a deploy actually serves, and it is where the two
# historical first-deploy failures (missing edge cert, dead Docker provider ->
# blanket 404) would show up.
echo "==> [2/5] Waiting for the edge to serve https://api.automail.local/healthz"
# --resolve is the shell-side equivalent of the hosts-file entry a real deploy
# needs (docs/deploy-checklist.md §6): it keeps the SNI and Host header set to the
# routed name while pointing the socket at the published port. --cacert (not -k)
# means a wrong/expired edge cert fails here rather than being waved through.
# NOTE: the path must be concatenated onto the URL -- passing it as a separate
# argument makes curl treat it as a second URL and the check silently never passes.
edge_curl() {
  local host="$1" path="$2"; shift 2
  curl -sS --cacert infra/traefik/edge-cert.pem \
    --resolve "${host}:${TRAEFIK_HTTPS_PORT}:127.0.0.1" \
    "$@" "https://${host}:${TRAEFIK_HTTPS_PORT}${path}"
}
ready=""
for i in $(seq 1 90); do
  code="$(edge_curl api.automail.local /healthz -o /dev/null -w '%{http_code}' 2>/dev/null || true)"
  if [ "$code" = "200" ]; then ready="1"; echo "   edge ready after ${i}s"; break; fi
  sleep 1
done
if [ -z "$ready" ]; then
  echo "!! the edge never served /healthz. Recent Traefik + cloud-server logs:" >&2
  "${COMPOSE[@]}" logs --tail=40 traefik cloud-server >&2 || true
  exit 1
fi

echo "==> [3/5] Waiting for the printer to report idle"
printer_live=""
for i in $(seq 1 30); do
  # Captured into a variable rather than piped into grep: under `set -o pipefail`
  # a short-circuiting reader can SIGPIPE the writer and turn a successful match
  # into a failed pipeline (the bug that corrupted every reading in T10's harness).
  state="$("${COMPOSE[@]}" exec -T redis redis-cli GET "mailbox:${DEV_MAILBOX_ID}:state" 2>/dev/null || true)"
  if [[ "$state" == *idle* ]]; then printer_live="1"; echo "   printer live after ${i}s"; break; fi
  sleep 1
done
if [ -z "$printer_live" ]; then
  echo "!! printer never reported idle; recent printer logs:" >&2
  "${COMPOSE[@]}" logs --tail=30 printer >&2 || true
  exit 1
fi

echo "==> [4/5] Seeding the fixture"
E2E_COMPOSE_FILES="-f docker-compose.yml -f docker-compose.deploy-smoke.yml" bash scripts/e2e/seed.sh

echo "==> [5/5] Driving the production profile through the HTTPS edge"
cd e2e
E2E_REPO_ROOT="$ROOT" \
E2E_COMPOSE_OVERRIDE="docker-compose.deploy-smoke.yml" \
SMOKE_HTTPS_PORT="$TRAEFIK_HTTPS_PORT" \
  go test -tags smoke -count=1 -v -timeout 5m ./...
