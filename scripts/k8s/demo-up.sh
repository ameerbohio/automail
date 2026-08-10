#!/usr/bin/env bash
# Public demo of the KUBERNETES deployment: the k3d cluster behind a Cloudflare
# quick tunnel, reachable from someone else's phone with real TLS. `make
# k8s-demo`. The Compose equivalent is scripts/demo/up.sh; this is the same
# idea against the cluster, and the two can run side by side.
#
# ============================ READ THIS FIRST =============================
# This publishes your cluster to the public internet. What protects it:
#   - The tunnel hostname is four random words. Unguessability IS the access
#     control -- a quick tunnel has no authentication in front of it.
#   - The seeded admin account (whose password is published in this repo) is
#     REPLACED with a freshly-registered one using a random password, before
#     the URL is printed, so the repo credential is never live.
#   - The guest rate limit and secure-headers middleware stay enabled.
# What does NOT protect it:
#   - Guest job submission is unauthenticated by design (rate-limited only).
#   - Object storage is reachable at /automail/ (ciphertext, SSE-S3 at rest,
#     but still reachable).
#   - The data is throwaway fixture data. Do not put anything real in it.
# Tear it down with `make k8s-demo-down` when finished; do not leave it running
# unattended.
# =========================================================================
#
# WHY THE HOSTNAME FORCES THIS TO BE A SCRIPT. Everything except one value is
# in infra/k8s/overlays/k3d-demo. That value is the tunnel hostname, which
# Cloudflare assigns only once cloudflared has connected -- and cloud-server
# must SIGN it into every pre-signed upload URL. So: apply, start the tunnel,
# read the hostname out of its log, rewire the pre-signer, roll. No manifest
# can hold it and no kustomize build can produce it.
#
# Env knobs:
#   PRINT=host           also start the printer against the host's CUPS queue
#   KEEP_DATA=1          skip the destructive re-apply (reuse existing PVCs)
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

# Exported before the source: lib-printer.sh reads PRINT at source time to
# decide which compose files PRINTER_COMPOSE holds, and teardown must be built
# from the same list as bring-up.
export PRINT="${PRINT:-}"
# shellcheck source=scripts/k8s/lib-printer.sh
source scripts/k8s/lib-printer.sh

OVERLAY=infra/k8s/overlays/k3d-demo
TUNNEL_CONTAINER=automail-k8s-tunnel
PUBLIC_HOST=""

tunnel_down() { docker rm -f "$TUNNEL_CONTAINER" >/dev/null 2>&1 || true; }

# Only tears down what THIS script started, and only on failure. A successful
# run must leave the tunnel and printer up -- that is the deliverable.
cleanup_on_fail() {
	local rc=$?
	[ "$rc" -eq 0 ] && return 0
	echo "!! bring-up failed (exit $rc) -- removing the tunnel so no half-wired" >&2
	echo "   public URL is left serving pre-signed URLs for the wrong host." >&2
	tunnel_down
	[ -n "$PRINT_MODE" ] && printer_down
	return "$rc"
}
trap cleanup_on_fail EXIT

echo "==> [1/7] Preflight"
command -v docker >/dev/null 2>&1 || fail "docker not found (needed for the cloudflared container)"
preflight_cluster   # also runs the host-CUPS checks when PRINT=host
echo "✔ cluster reachable, workloads Ready"

echo "==> [2/7] Applying the demo overlay (one origin, plain HTTP, path-routed)"
# The demo overlay only changes the edge and the pre-signer's origin, so an
# existing cluster rolls onto it in place -- no PVC is touched and the data
# tier does not restart. That is why there is no `down -v` equivalent here and
# no ALLOW_DESTRUCTIVE guard: unlike the Compose demo, this one is additive.
kubectl apply -k "$OVERLAY" >/dev/null
kc rollout status deploy/portal --timeout=180s >/dev/null
echo "✔ overlay applied"

echo "==> [3/7] Starting the Cloudflare tunnel"
tunnel_down
# --network host so `localhost:9080` means the k3d loadbalancer's published
# port (infra/k8s/k3d-cluster.yaml maps 9080 -> Traefik :80). Pointed at the
# HTTP entrypoint deliberately: Cloudflare terminates TLS at its edge, and
# sniStrict on `websecure` would reject a *.trycloudflare.com SNI outright,
# since no SAN in the self-signed edge cert matches it.
#
# NOT `--restart unless-stopped`: a quick tunnel gets a FRESH random hostname
# on every start, so an automatic restart would serve a new URL while the one
# printed below goes dead -- and cloud-server would still be signing upload
# URLs for the old host, so submissions would fail with nothing obviously
# wrong. Failing visibly and making the operator re-run is safer.
docker run -d --name "$TUNNEL_CONTAINER" --network host --restart no \
	cloudflare/cloudflared:latest \
	tunnel --no-autoupdate --url http://localhost:"$EDGE_HTTP_PORT" >/dev/null

for i in $(seq 1 60); do
	PUBLIC_HOST="$(docker logs "$TUNNEL_CONTAINER" 2>&1 |
		grep -oE '[a-z0-9-]+\.trycloudflare\.com' | head -1 || true)"
	[ -n "$PUBLIC_HOST" ] && { echo "✔ assigned after ${i}s: $PUBLIC_HOST"; break; }
	sleep 1
done
[ -n "$PUBLIC_HOST" ] || {
	docker logs --tail=30 "$TUNNEL_CONTAINER" >&2 || true
	fail "the tunnel never reported a hostname"
}
PUBLIC_URL="https://${PUBLIC_HOST}"

echo "==> [4/7] Pointing the upload pre-signer at $PUBLIC_HOST"
# `kubectl set env` replaces the container's configMapKeyRef for these keys
# with literals -- an explicit env entry wins over the ConfigMap reference --
# and the spec change is what rolls the Deployment onto them. This is the step
# the placeholder in the overlay exists for.
#
# No port in the endpoint: Cloudflare serves 443, and MinIO's presigner builds
# <endpoint>/<bucket>/<key>, so `<host>` yields https://<host>/automail/<key>
# -- exactly the path demo-minio routes.
kc set env deployment/cloud-server \
	MINIO_PUBLIC_ENDPOINT="$PUBLIC_HOST" \
	MINIO_PUBLIC_SECURE=true >/dev/null
kc rollout status deploy/cloud-server --timeout=180s >/dev/null
echo "✔ pre-signer rewired and rolled"

echo "==> [5/7] Waiting for the public URL"
# A 200 on the portal root proves the whole chain: Cloudflare edge ->
# cloudflared -> k3d loadbalancer -> Traefik -> portal pod. Accepting any
# status would call a 404 "ready" and hide a broken route.
#
# curl -4: Cloudflare publishes AAAA records, and a host without working IPv6
# egress fails the connect instead of falling back. Browsers do Happy Eyeballs
# and fall back on their own, so this only affects checks made from here.
ready=""
for i in $(seq 1 90); do
	code="$(curl -4 -sS -o /dev/null -w '%{http_code}' --max-time 10 "$PUBLIC_URL/" 2>/dev/null || true)"
	[ "$code" = "200" ] && { ready=1; echo "✔ serving after ${i}s"; break; }
	sleep 1
done
[ -n "$ready" ] || fail "the public URL never responded" \
	"check: docker logs $TUNNEL_CONTAINER, and kubectl -n $NAMESPACE get ingressroute"

echo "==> [6/7] Seeding the fixture and replacing the seeded admin"
SEED_BACKEND=kubectl bash scripts/e2e/seed.sh >/dev/null

# scripts/e2e/seed.sh inserts admin@automail.test with a bcrypt hash of a
# password printed in this repo -- fine for a local fixture, not fine on a
# public URL. Registering through the product's own endpoint means the new
# password is hashed by the real code path (no bcrypt tooling needed here), and
# the role is promoted directly because admin is deliberately not
# self-assignable through the API.
ADMIN_EMAIL="demo-admin-$(date +%s)@automail.test"
# NOT `tr -dc ... </dev/urandom | head -c 20`: head closes the pipe, tr dies of
# SIGPIPE, and under `set -o pipefail` that aborts the script (exit 141).
ADMIN_PW="$(openssl rand -base64 32 | tr -dc 'A-Za-z0-9' | cut -c1-20)"
code="$(curl -4 -sS -o /dev/null -w '%{http_code}' --max-time 20 -X POST \
	-H 'Content-Type: application/json' \
	-d "{\"email\":\"${ADMIN_EMAIL}\",\"password\":\"${ADMIN_PW}\"}" \
	"$PUBLIC_URL/api/auth/register" || true)"
[ "$code" = "200" ] || [ "$code" = "201" ] || fail \
	"could not register the demo admin (status $code)" \
	"the stack is up but the repo's published admin password is live -- run \`make k8s-demo-down\`"

PGPASS="$(grep '^POSTGRES_PASSWORD=' .env | cut -d= -f2-)"
kc exec -i postgres-0 -- env PGPASSWORD="$PGPASS" \
	psql -v ON_ERROR_STOP=1 -q \
	-U "$(grep '^POSTGRES_USER=' .env | cut -d= -f2-)" \
	-d "$(grep '^POSTGRES_DB=' .env | cut -d= -f2-)" \
	-v email="$ADMIN_EMAIL" \
	-v app_key="$(grep '^APP_ENCRYPTION_KEY=' .env | cut -d= -f2-)" <<'SQL'
UPDATE senders SET role = 'admin'
 WHERE pgp_sym_decrypt(email_enc, :'app_key') = :'email';
DELETE FROM senders WHERE id = '44444444-4444-4444-4444-444444444444';
SQL
echo "✔ fixture seeded, repo admin credential retired"

echo "==> [7/7] Printer"
# On by default: a demo whose jobs never leave `queued` is not a demo. Set
# START_PRINTER=0 to bring the public URL up without one (e.g. the printer is
# already running from `make k8s-printer-up`).
if [ "${START_PRINTER:-1}" = "1" ]; then
	resolve_minio_host
	start_printer
	wait_printer_idle
	owner="$(find_socket_owner)"
else
	owner="(printer not started)"
fi

if [ "$PRINT_MODE" = "host" ]; then
	PRINT_BANNER="ENABLED -- jobs print for real on queue '$PRINTER_NAME'"
else
	PRINT_BANNER="off (DEV_MODE) -- jobs reach \"delivered\", no paper. PRINT=host to enable."
fi

trap - EXIT
cat <<EOF

  ┌─────────────────────────────────────────────────────────────────────┐
     AUTOMAIL KUBERNETES DEMO IS LIVE

     URL       ${PUBLIC_URL}
     Search    Testmann   (seeded resident, shown masked as "R. Testmann")

     Admin     ${ADMIN_EMAIL}
     Password  ${ADMIN_PW}

     Real Cloudflare TLS, so no certificate warnings on anyone's device.
     Printing  ${PRINT_BANNER}
     Socket    ${owner}

     Pods:       kubectl -n ${NAMESPACE} get pods -o wide
     Printer:    docker logs -f ${PRINTER_CONTAINER}
     SHUT DOWN:  make k8s-demo-down
  └─────────────────────────────────────────────────────────────────────┘

  This URL is public to anyone who has it. Do not leave it running unattended.
  The slot fills after 5 jobs (printer process, Max 5) -- reset it without
  disturbing the tunnel or the URL:
      make k8s-printer-down && PRINT=${PRINT_MODE:-} make k8s-printer-up

EOF
