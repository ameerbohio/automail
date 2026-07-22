#!/usr/bin/env bash
# One-shot public demo: brings the whole stack up behind a Cloudflare quick
# tunnel and prints a URL you can open on a phone. `make demo`.
#
# ============================ READ THIS FIRST =============================
# This publishes your machine to the public internet. What protects it:
#   - The tunnel hostname is four random words. Unguessability IS the access
#     control -- a quick tunnel has no authentication in front of it.
#   - The seeded admin account (whose password is published in this repo) is
#     REPLACED with a freshly-registered one using a random password. That
#     happens before the URL is printed, so the repo credential is never live.
#   - The guest rate limit and secure-headers middleware stay enabled.
# What does NOT protect it:
#   - Guest job submission is unauthenticated by design (rate-limited only).
#   - Object storage is reachable at /automail/ (ciphertext + SSE-S3 at rest,
#     but still reachable).
#   - The data is throwaway fixture data. Do not put anything real in it.
# Tear it down with `make demo-down` when you are finished, and do not leave it
# running unattended.
# =========================================================================
#
# Destructive, like scripts/deploy/smoke.sh: it runs `down -v` first so every run
# starts from a clean, known fixture. Refuses if it finds an existing Postgres
# volume unless ALLOW_DESTRUCTIVE=1.
#
# Env knobs:
#   NO_BUILD=1           skip the image rebuild (faster re-runs)
#   ALLOW_DESTRUCTIVE=1  proceed even though an existing data volume is present
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
ROOT="$(pwd)"

COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.demo.yml)
fail() { echo "!! $1" >&2; [ -n "${2:-}" ] && echo "   fix: $2" >&2; exit 1; }

# PRINT=1 layers the print-enabled override: DEV_MODE off, so the printer makes
# the REAL `lp` call, against a CUPS server running as a container (see
# docker-compose.demo-print.yml for why not the host's).
#
# PRINT=host instead uses the host's cupsd through the mounted socket -- the
# Proxmox arrangement (docs/cups-host-setup.md). Only that mode needs the host
# checks below.
#
# Either way the failure to guard against is the same: a missing or misnamed
# queue does not stop the stack coming up, it just makes every job fail one at a
# time, which is miserable to debug from a phone.
PRINTER_NAME="${PRINTER_NAME:-Canon_MF240}"
export PRINTER_NAME
case "${PRINT:-}" in
  1)    COMPOSE+=(-f docker-compose.demo-print.yml) ;;
  host) COMPOSE+=(-f docker-compose.demo-print.yml)
        export CUPS_SERVER=""   # fall back to /run/cups/cups.sock
        ;;
esac

# preflight_host_cups only applies to PRINT=host: no cupsd on the host means
# nothing downstream can work, and saying so before building is much clearer.
preflight_host_cups() {
  if [ -d /run/cups/cups.sock ]; then
    fail "/run/cups/cups.sock is a DIRECTORY, not a socket -- Docker auto-created it because no cupsd was running" \
         "sudo rmdir /run/cups/cups.sock && sudo systemctl start cups"
  fi
  [ -S /run/cups/cups.sock ] || fail \
    "no CUPS socket at /run/cups/cups.sock -- there is no print server on this host" \
    "sudo apt install -y cups cups-client && sudo systemctl start cups   (docs/cups-host-setup.md), or use PRINT=1 for the containerised CUPS"
  command -v lpstat >/dev/null 2>&1 || return 0
  lpstat -p "$PRINTER_NAME" >/dev/null 2>&1 || fail \
    "cupsd is running but has no queue named '${PRINTER_NAME}'" \
    "run \`lpstat -p -d\` to see the real queue name, then re-run with PRINTER_NAME=<name>"
}

# verify_container_can_print runs AFTER start and is the definitive check for both
# modes: a queue existing somewhere does not prove the PRINTER CONTAINER can reach
# it, and that hop (socket mount, or CUPS_SERVER over the network) is exactly what
# breaks. Asking lp's own client inside the container tests the real path a job
# will take.
verify_container_can_print() {
  local i
  for i in $(seq 1 30); do
    if "${COMPOSE[@]}" exec -T printer lpstat -p "$PRINTER_NAME" >/dev/null 2>&1; then
      echo "   printer container can reach CUPS queue '${PRINTER_NAME}'"
      return 0
    fi
    sleep 1
  done
  echo "!! the printer CONTAINER cannot see queue '${PRINTER_NAME}'" >&2
  "${COMPOSE[@]}" exec -T printer lpstat -p 2>&1 | head -5 >&2 || true
  echo "   every job would fail instead of printing, so the stack is being torn down." >&2
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  exit 1
}

# Traefik still binds host ports in the base compose even though the tunnel
# reaches it over the compose network. Move them off 80/443 so the demo does not
# fight anything already running and does not need privileged ports.
export TRAEFIK_HTTP_PORT="${TRAEFIK_HTTP_PORT:-8080}"
export TRAEFIK_HTTPS_PORT="${TRAEFIK_HTTPS_PORT:-8443}"

echo "==> [1/6] Preflight"
docker info >/dev/null 2>&1 || fail "no Docker daemon" "start Docker and re-run"
[ "${PRINT:-}" = "host" ] && preflight_host_cups && echo "   host CUPS reachable"
bash scripts/e2e/bootstrap.sh >/dev/null
if docker volume inspect "$(basename "$ROOT")_pg_data" >/dev/null 2>&1 \
   && [ "${ALLOW_DESTRUCTIVE:-}" != "1" ]; then
  fail "an existing Postgres volume was found -- this script runs \`down -v\` and would destroy it" \
       "re-run with ALLOW_DESTRUCTIVE=1 if that data is disposable"
fi
"${COMPOSE[@]}" config >/dev/null || fail "compose config does not resolve" "check .env against .env.example"

echo "==> [2/6] Bringing up a clean stack + tunnel"
"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
if [ "${NO_BUILD:-}" = "1" ]; then "${COMPOSE[@]}" up -d; else "${COMPOSE[@]}" up -d --build; fi

# The chicken-and-egg this script exists to solve: the pre-signed upload URL must
# name the tunnel hostname, but that hostname is assigned by Cloudflare only once
# cloudflared has connected. So: read it out of the tunnel's log, then recreate
# cloud-server with it. Traefik needs no recreate because the demo routers match
# on path, not Host.
echo "==> [3/6] Waiting for Cloudflare to assign a hostname"
PUBLIC_HOST=""
for i in $(seq 1 60); do
  log="$("${COMPOSE[@]}" logs cloudflared 2>/dev/null || true)"
  host="$(grep -oE '[a-z0-9-]+\.trycloudflare\.com' <<<"$log" | head -1 || true)"
  if [ -n "$host" ]; then PUBLIC_HOST="$host"; echo "   assigned after ${i}s: ${PUBLIC_HOST}"; break; fi
  sleep 1
done
[ -n "$PUBLIC_HOST" ] || {
  echo "!! the tunnel never reported a hostname; recent cloudflared logs:" >&2
  "${COMPOSE[@]}" logs --tail=30 cloudflared >&2 || true
  exit 1
}
PUBLIC_URL="https://${PUBLIC_HOST}"

echo "==> [4/6] Pointing the upload pre-signer at ${PUBLIC_HOST}"
export MINIO_PUBLIC_ENDPOINT="$PUBLIC_HOST"
export MINIO_PUBLIC_SECURE=true
export MINIO_CORS_ORIGIN="$PUBLIC_URL"
"${COMPOSE[@]}" up -d --no-deps --no-build cloud-server >/dev/null

# A 200 on the portal root proves the entire chain end to end: Cloudflare edge ->
# cloudflared -> Traefik router -> portal. Anything weaker (accepting any status)
# would call a 404 "ready" and hide a broken route.
#
# curl -4 is required, not cosmetic: Cloudflare publishes AAAA records, and a
# host without working IPv6 egress (WSL2 by default) fails the connect in ~20ms
# instead of falling back, so this loop would flap or time out against a tunnel
# that is perfectly healthy. Real clients -- browsers -- do Happy Eyeballs and
# fall back on their own, so this only affects checks made from this host.
ready=""
for i in $(seq 1 90); do
  code="$(curl -4 -sS -o /dev/null -w '%{http_code}' --max-time 10 "${PUBLIC_URL}/" 2>/dev/null || true)"
  if [ "$code" = "200" ]; then ready=1; echo "   public URL serving after ${i}s"; break; fi
  sleep 1
done
[ -n "$ready" ] || fail "the public URL never responded" "check \`${COMPOSE[*]} logs cloudflared traefik\`"

[ -n "${PRINT:-}" ] && verify_container_can_print

echo "==> [5/6] Seeding the fixture"
E2E_COMPOSE_FILES="-f docker-compose.yml -f docker-compose.demo.yml" bash scripts/e2e/seed.sh >/dev/null

# Replace the seeded admin. scripts/e2e/seed.sh inserts admin@automail.test with
# a bcrypt hash of a password printed in this repo -- fine for a local fixture,
# not fine on a public URL. Registering through the product's own /auth/register
# means the new password is hashed by the real code path (no bcrypt tooling here),
# and the role is promoted directly because admin is deliberately not
# self-assignable through the API.
echo "==> [6/6] Replacing the seeded admin credential"
ADMIN_EMAIL="demo-admin-$(date +%s)@automail.test"
# NOT `tr -dc ... </dev/urandom | head -c 20`: head closes the pipe after 20
# bytes, tr dies of SIGPIPE, and under `set -o pipefail` that aborts the whole
# script (exit 141) -- the same trap that silently corrupted the T10 load
# harness. openssl writes a small bounded amount and cut reads to EOF, so no
# reader ever closes early.
ADMIN_PW="$(openssl rand -base64 32 | tr -dc 'A-Za-z0-9' | cut -c1-20)"
code="$(curl -4 -sS -o /dev/null -w '%{http_code}' --max-time 20 -X POST \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"${ADMIN_EMAIL}\",\"password\":\"${ADMIN_PW}\"}" \
  "${PUBLIC_URL}/api/auth/register" || true)"
[ "$code" = "200" ] || [ "$code" = "201" ] || fail "could not register the demo admin (status ${code})" \
  "the stack is up but has the repo's published admin password live -- run \`make demo-down\`"

PG=(-e PGPASSWORD="$(sed -n 's/^POSTGRES_PASSWORD=//p' .env | tail -1)")
PSQL=(psql -v ON_ERROR_STOP=1 -U "$(sed -n 's/^POSTGRES_USER=//p' .env | tail -1)" \
      -d "$(sed -n 's/^POSTGRES_DB=//p' .env | tail -1)")
APP_KEY="$(sed -n 's/^APP_ENCRYPTION_KEY=//p' .env | tail -1)"
"${COMPOSE[@]}" exec -T "${PG[@]}" postgres "${PSQL[@]}" -q \
  -v email="$ADMIN_EMAIL" -v app_key="$APP_KEY" <<'SQL'
-- Promote the freshly-registered account, then delete the fixture admin whose
-- password is public in this repo.
UPDATE senders SET role = 'admin'
 WHERE pgp_sym_decrypt(email_enc, :'app_key') = :'email';
DELETE FROM senders WHERE id = '44444444-4444-4444-4444-444444444444';
SQL

if [ -n "${PRINT:-}" ]; then
  PRINT_BANNER="ENABLED -- jobs print for real on queue '${PRINTER_NAME}'"
else
  PRINT_BANNER="off (DEV_MODE) -- jobs reach \"delivered\", no paper. PRINT=1 to enable."
fi

cat <<EOF

  ┌─────────────────────────────────────────────────────────────────────┐
     AUTOMAIL DEMO IS LIVE

     URL       ${PUBLIC_URL}
     Search    Testmann   (seeded resident, shown masked as "R. Testmann")

     Admin     ${ADMIN_EMAIL}
     Password  ${ADMIN_PW}

     Real Cloudflare TLS, so no certificate warnings on a phone.
     Printing  ${PRINT_BANNER}

     Follow the printer:  ${COMPOSE[*]} logs -f printer
     SHUT IT DOWN:        make demo-down
  └─────────────────────────────────────────────────────────────────────┘

  This URL is public to anyone who has it. Do not leave it running unattended.

EOF
