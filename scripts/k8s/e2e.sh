#!/usr/bin/env bash
# Goal K5 acceptance: a job submitted through the Kubernetes ingress reaches
# `delivered` on a printer that lives OUTSIDE the cluster, /dev/shm is clean
# afterwards, and the Redis fan-in across cloud-server pods is proved.
# Spec: plans/16-kubernetes.md §6. Run with `make k8s-e2e`.
#
# WHAT THIS SCRIPT OWNS AND WHAT THE GO DRIVER OWNS. Everything that needs
# `kubectl` or `docker` lives here (most of it in scripts/k8s/lib-printer.sh,
# shared with the Goal K6 failure suite); the driver (tests/system/k8s_test.go, build
# tag `k8s`) is pure HTTP against addresses this script resolves and exports.
# That split is not cosmetic — §6.2 says the goal must *state* how it targets a
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
# shellcheck source=scripts/k8s/lib-printer.sh
source scripts/k8s/lib-printer.sh

FORWARD_PORT="${FORWARD_PORT:-18080}"
FORWARD_PID=""

cleanup() {
	[ -n "$FORWARD_PID" ] && kill "$FORWARD_PID" 2>/dev/null || true
	if [ "${KEEP_PRINTER:-}" = "1" ]; then
		echo "==> KEEP_PRINTER=1: leaving $PRINTER_CONTAINER up"
		return
	fi
	printer_down
}
trap cleanup EXIT

# --- 0. preflight ---------------------------------------------------------
preflight_cluster

# --- 1. object storage must be reachable from outside the cluster ---------
resolve_minio_host

# --- 2. seed the cluster's Postgres ---------------------------------------
# Same fixture and same .env values as every Compose suite; only the exec
# backend differs. That is the whole point of seed.sh's SEED_BACKEND knob.
SEED_BACKEND=kubectl bash scripts/e2e/seed.sh

# --- 3. the printer, outside the cluster ----------------------------------
start_printer
wait_printer_idle

# --- 4. who owns the socket? ----------------------------------------------
owner="$(find_socket_owner)"
echo "✔ printer socket owner: $owner"

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
cd tests/system
E2E_REPO_ROOT="$ROOT" \
	E2E_PRINTER_CONTAINER="$PRINTER_CONTAINER" \
	K8S_EDGE_HTTPS_PORT="$EDGE_HTTPS_PORT" \
	K8S_OWNER_POD="$owner" \
	K8S_NONOWNER_POD="$non_owner" \
	K8S_NONOWNER_URL="http://127.0.0.1:$FORWARD_PORT" \
	go test -tags k8s -count=1 -v -timeout 10m ./...
