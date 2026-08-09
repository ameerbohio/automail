#!/usr/bin/env bash
# Goal K5 acceptance: a job submitted through the Kubernetes ingress reaches
# `delivered` on a printer that lives OUTSIDE the cluster, /dev/shm is clean
# afterwards, and the Redis fan-in across cloud-server pods is proved.
# Spec: plans/16-kubernetes.md §6. Run with `make k8s-e2e`.
#
# WHAT THIS SCRIPT OWNS AND WHAT THE GO DRIVER OWNS. Everything that needs
# `kubectl` or `docker` lives here; the driver (e2e/k8s_test.go, build tag
# `k8s`) is pure HTTP against addresses this script resolves and exports. That
# split is not cosmetic — §6.2 says the goal must *state* how it targets a
# specific pod when a Service refuses to be addressed per-pod, and the answer
# ("find the socket owner in the pods' logs, port-forward to a different one")
# is inherently kubectl work.
#
# Env knobs:
#   KEEP_PRINTER=1   leave the printer container up afterwards (debugging)
#   NO_BUILD=1       skip the printer image rebuild
#   FORWARD_PORT     host port for the non-owner pod forward (default 18080)
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
ROOT="$(pwd)"
source scripts/k8s/versions.env

# Absolute paths, not relative: the driver runs from ./e2e (Go modules), so the
# EXIT trap fires with that as its cwd and a relative -f would silently fail to
# find the file — leaving the printer container running after every run.
PRINTER_COMPOSE=(docker compose --project-directory "$ROOT" -f "$ROOT/docker-compose.k8s-printer.yml")
PRINTER_CONTAINER=automail-k8s-printer
FORWARD_PORT="${FORWARD_PORT:-18080}"
MAILBOX_ID="${DEV_MAILBOX_ID:-00000000-0000-0000-0000-000000000001}"
FORWARD_PID=""

fail() {
	echo "✗ $1" >&2
	[ $# -gt 1 ] && echo "  → $2" >&2
	exit 1
}
kc() { kubectl -n "$NAMESPACE" "$@"; }

cleanup() {
	# Teardown must not depend on the guard that protects bring-up: the compose
	# file requires K8S_MINIO_HOST_IP so a misconfigured `up` fails loudly
	# instead of printing blobs from nowhere, but Compose interpolates on `down`
	# too, and a cleanup that cannot run is worse than an unvalidated one.
	export K8S_MINIO_HOST_IP="${K8S_MINIO_HOST_IP:-0.0.0.0}"
	[ -n "$FORWARD_PID" ] && kill "$FORWARD_PID" 2>/dev/null || true
	if [ "${KEEP_PRINTER:-}" = "1" ]; then
		echo "==> KEEP_PRINTER=1: leaving $PRINTER_CONTAINER up"
		return
	fi
	"${PRINTER_COMPOSE[@]}" down --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- 0. preflight ---------------------------------------------------------
command -v kubectl >/dev/null 2>&1 || fail "kubectl not found" "make k8s-tools"
[ -f .env ] || fail ".env is missing" "cp .env.example .env and fill it in"
[ -r infra/certs/printer-cert.pem ] || fail "infra/certs/printer-cert.pem missing" "./infra/certs/gen.sh"
kc get deploy cloud-server >/dev/null 2>&1 || fail "deployment/cloud-server not found" "make k8s-up && make k8s-images && make k8s-secrets && make k8s-apply"
kc rollout status deploy/cloud-server --timeout=120s >/dev/null
kc rollout status statefulset/minio --timeout=120s >/dev/null

# --- 1. object storage must be reachable from outside the cluster ---------
# The dispatch frame's pre-signed GET URL is signed by cloud-server's INTERNAL
# MinIO client, so its host is `minio:9000` and SigV4 has signed that name and
# port into the request. The k3d overlay therefore gives the MinIO pod a
# hostPort inside its k3d node container, whose IP is routable from this host;
# the printer maps the name onto it. Resolve the IP now — Docker assigns it, so
# it moves whenever the k3d network is recreated and must never be hardcoded.
MINIO_HOST_IP="$(kc get pod minio-0 -o jsonpath='{.status.hostIP}')"
[ -n "$MINIO_HOST_IP" ] || fail "could not resolve minio-0's node IP"
curl -sf -o /dev/null --max-time 5 "http://$MINIO_HOST_IP:9000/minio/health/live" ||
	fail "MinIO is not answering on $MINIO_HOST_IP:9000" \
		"the k3d-local overlay's hostPort patch is what publishes it — re-run \`make k8s-apply\`"
echo "✔ object storage reachable from the host at $MINIO_HOST_IP:9000 (k3d node hostPort)"
export K8S_MINIO_HOST_IP="$MINIO_HOST_IP"

# --- 2. seed the cluster's Postgres ---------------------------------------
# Same fixture and same .env values as every Compose suite; only the exec
# backend differs. That is the whole point of seed.sh's SEED_BACKEND knob.
SEED_BACKEND=kubectl bash scripts/e2e/seed.sh

# --- 3. the printer, outside the cluster ----------------------------------
# Recreated every run rather than reused: the dev printer's slot occupancy only
# ever increments in memory, so a long-lived container eventually fills the
# seeded slot (max_count 5) and jobs stop dispatching with nothing logged as an
# error. A fresh container starts at zero.
echo "==> Starting the printer (outside the cluster, dialing wss://localhost:$MTLS_HOST_PORT)"
"${PRINTER_COMPOSE[@]}" down --remove-orphans >/dev/null 2>&1 || true
started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
if [ "${NO_BUILD:-}" = "1" ]; then
	"${PRINTER_COMPOSE[@]}" up -d >/dev/null
else
	"${PRINTER_COMPOSE[@]}" up -d --build >/dev/null
fi

# The register frame must be acked and the state cache seeded before a job is
# submitted, or the first dispatch finds no live printer and the job queues —
# which would make the fan-in assertion (status == "dispatching") fail for a
# reason that has nothing to do with fan-in. hub.go seeds mailbox:<id>:state
# only after the ack, precisely so callers have this signal to poll on.
echo "==> Waiting for the printer to register with a cloud-server pod"
registered=""
for i in $(seq 1 60); do
	if kc exec redis-0 -- redis-cli GET "mailbox:${MAILBOX_ID}:state" 2>/dev/null | grep -q idle; then
		registered=1
		echo "    printer idle after ${i}s"
		break
	fi
	sleep 1
done
[ -n "$registered" ] || {
	docker logs "$PRINTER_CONTAINER" 2>&1 | tail -20 >&2
	fail "the printer never reported idle" \
		"check the mTLS dial to wss://localhost:$MTLS_HOST_PORT (host mapping frozen in infra/k8s/k3d-cluster.yaml)"
}

# --- 4. who owns the socket? ----------------------------------------------
# kube-proxy chose, so this has to be discovered rather than assumed (§6.2).
# The window starts at the container's creation, so a registration line inside
# it belongs to THIS printer and not to some earlier run; the newest line wins
# if the printer reconnected mid-run (a rollout would do that).
owner="" owner_ts=""
for pod in $(kc get pods -l app.kubernetes.io/name=cloud-server -o jsonpath='{.items[*].metadata.name}'); do
	line="$(kc logs "$pod" --timestamps --since-time="$started_at" 2>/dev/null |
		grep -F "printer-link: mailbox ${MAILBOX_ID} registered" | tail -1 || true)"
	[ -n "$line" ] || continue
	ts="${line%% *}"
	if [ -z "$owner_ts" ] || [[ "$ts" > "$owner_ts" ]]; then
		owner="$pod"
		owner_ts="$ts"
	fi
done
[ -n "$owner" ] || fail "no cloud-server pod logged a registration for mailbox $MAILBOX_ID since $started_at" \
	"the printer reported idle, so the socket exists — did the pods restart in between?"
echo "✔ printer socket owner: $owner (registered $owner_ts)"

# --- 5. a pod that is NOT the owner, addressed directly -------------------
non_owner=""
for pod in $(kc get pods -l app.kubernetes.io/name=cloud-server -o jsonpath='{.items[*].metadata.name}'); do
	[ "$pod" = "$owner" ] || {
		non_owner="$pod"
		break
	}
done
[ -n "$non_owner" ] || fail "only one cloud-server pod exists, so there is no non-owner to submit to" \
	"scale the Deployment back to 3 (make k8s-apply)"

# A Service load-balances and cannot be addressed per-pod, so the fan-in test
# would otherwise depend on load-balancer luck. port-forward is the lever §6.2
# names; the driver re-checks X-Automail-Node before trusting it.
kubectl -n "$NAMESPACE" port-forward "pod/$non_owner" "$FORWARD_PORT:8080" >/dev/null 2>&1 &
FORWARD_PID=$!
for i in $(seq 1 30); do
	curl -sf -o /dev/null --max-time 2 "http://127.0.0.1:$FORWARD_PORT/livez" && break
	kill -0 "$FORWARD_PID" 2>/dev/null || fail "kubectl port-forward to $non_owner exited" \
		"is host port $FORWARD_PORT already in use? set FORWARD_PORT="
	[ "$i" = 30 ] && fail "port-forward to $non_owner never answered on $FORWARD_PORT"
	sleep 1
done
echo "✔ non-owner pod $non_owner addressable at http://127.0.0.1:$FORWARD_PORT"

# --- 6. drive it ----------------------------------------------------------
echo "==> Running the Kubernetes E2E driver"
cd e2e
E2E_REPO_ROOT="$ROOT" \
	E2E_PRINTER_CONTAINER="$PRINTER_CONTAINER" \
	K8S_EDGE_HTTPS_PORT="$EDGE_HTTPS_PORT" \
	K8S_OWNER_POD="$owner" \
	K8S_NONOWNER_POD="$non_owner" \
	K8S_NONOWNER_URL="http://127.0.0.1:$FORWARD_PORT" \
	go test -tags k8s -count=1 -v -timeout 10m ./...
