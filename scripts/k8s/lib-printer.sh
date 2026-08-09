# Shared bring-up for the two suites that drive the cluster with a printer
# living OUTSIDE it (Goals K5 and K6, plans/16-kubernetes.md §6).
#
# SOURCE THIS, DO NOT EXECUTE IT. The caller is expected to have `set -euo
# pipefail` and to have cd'd to the repo root; every function below assumes
# both. It defines the fixed names/paths, the fail/kc helpers, and the four
# steps that are identical between the two suites: resolve object storage,
# start the printer, wait for it to register, and discover which cloud-server
# pod ended up holding its socket.
#
# Why a library rather than a second copy: the printer bring-up is not boring
# boilerplate — it encodes three findings that took a goal each to learn (the
# pre-signed URL is signed against `minio:9000` so the printer must map that
# name onto the node hostPort; the dev printer's slot occupancy only ever
# increments so the container must be recreated per run; the socket owner is
# whatever pod kube-proxy picked, so it has to be read out of the logs). A
# divergent copy would quietly lose one of them.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/k8s/versions.env
source "$ROOT/scripts/k8s/versions.env"

# Absolute paths, not relative: the Go drivers run from ./e2e (its own module),
# so a caller's EXIT trap fires with that as its cwd and a relative -f would
# silently fail to find the file — leaving the printer container running after
# every run.
PRINTER_COMPOSE=(docker compose --project-directory "$ROOT" -f "$ROOT/infra/compose/k8s-printer.yml")
PRINTER_CONTAINER=automail-k8s-printer
MAILBOX_ID="${DEV_MAILBOX_ID:-00000000-0000-0000-0000-000000000001}"
# Set by start_printer; the log scan in find_socket_owner is bounded by it so a
# registration line from an earlier run can never be mistaken for this one's.
PRINTER_STARTED_AT=""

fail() {
	echo "✗ $1" >&2
	[ $# -gt 1 ] && echo "  → $2" >&2
	exit 1
}

kc() { kubectl -n "$NAMESPACE" "$@"; }

# printer_down tears the printer's Compose project down. Teardown must not
# depend on the guard that protects bring-up: the compose file requires
# K8S_MINIO_HOST_IP so a misconfigured `up` fails loudly instead of printing
# blobs from nowhere, but Compose interpolates on `down` too, and a cleanup
# that cannot run is worse than an unvalidated one.
printer_down() {
	export K8S_MINIO_HOST_IP="${K8S_MINIO_HOST_IP:-0.0.0.0}"
	"${PRINTER_COMPOSE[@]}" down --remove-orphans >/dev/null 2>&1 || true
}

# preflight_cluster asserts the tooling, the secrets and a healthy workload are
# all present before anything destructive or slow starts.
preflight_cluster() {
	command -v kubectl >/dev/null 2>&1 || fail "kubectl not found" "make k8s-tools"
	[ -f .env ] || fail ".env is missing" "cp .env.example .env and fill it in"
	[ -r infra/certs/printer-cert.pem ] || fail "infra/certs/printer-cert.pem missing" "./infra/certs/gen.sh"
	kc get deploy cloud-server >/dev/null 2>&1 || fail "deployment/cloud-server not found" \
		"make k8s-up && make k8s-images && make k8s-secrets && make k8s-apply"
	kc rollout status deploy/cloud-server --timeout=120s >/dev/null
	kc rollout status statefulset/minio --timeout=120s >/dev/null
}

# resolve_minio_host exports K8S_MINIO_HOST_IP, the address the printer maps
# the name `minio` onto.
#
# The dispatch frame's pre-signed GET URL is signed by cloud-server's INTERNAL
# MinIO client, so its host is `minio:9000` and SigV4 has signed that name and
# port into the request. The k3d overlay therefore gives the MinIO pod a
# hostPort inside its k3d node container, whose IP is routable from this host.
# Docker assigns that IP, so it moves whenever the k3d network is recreated and
# must never be hardcoded.
resolve_minio_host() {
	local ip
	ip="$(kc get pod minio-0 -o jsonpath='{.status.hostIP}')"
	[ -n "$ip" ] || fail "could not resolve minio-0's node IP"
	curl -sf -o /dev/null --max-time 5 "http://$ip:9000/minio/health/live" ||
		fail "MinIO is not answering on $ip:9000" \
			"the k3d-local overlay's hostPort patch is what publishes it — re-run \`make k8s-apply\`"
	echo "✔ object storage reachable from the host at $ip:9000 (k3d node hostPort)"
	export K8S_MINIO_HOST_IP="$ip"
}

# start_printer recreates the printer container outside the cluster. Recreated
# every run rather than reused: the dev printer's slot occupancy only ever
# increments in memory (print.go newSlotState, Max 5) and resets solely when
# the printer PROCESS restarts, so a long-lived container eventually fills the
# seeded slot and jobs stop dispatching with nothing logged as an error.
start_printer() {
	echo "==> Starting the printer (outside the cluster, dialing wss://localhost:$MTLS_HOST_PORT)"
	printer_down
	PRINTER_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	if [ "${NO_BUILD:-}" = "1" ]; then
		"${PRINTER_COMPOSE[@]}" up -d >/dev/null
	else
		"${PRINTER_COMPOSE[@]}" up -d --build >/dev/null
	fi
}

# wait_printer_idle blocks until the printer has registered AND been acked.
# The register frame must be acked and the state cache seeded before a job is
# submitted, or the first dispatch finds no live printer and the job queues —
# which would make a fan-in assertion fail for a reason that has nothing to do
# with fan-in. hub.go seeds mailbox:<id>:state only after the ack, precisely so
# callers have this signal to poll on.
wait_printer_idle() {
	echo "==> Waiting for the printer to register with a cloud-server pod"
	local i
	for i in $(seq 1 60); do
		if kc exec redis-0 -- redis-cli GET "mailbox:${MAILBOX_ID}:state" 2>/dev/null | grep -q idle; then
			echo "    printer idle after ${i}s"
			return 0
		fi
		sleep 1
	done
	docker logs "$PRINTER_CONTAINER" 2>&1 | tail -20 >&2
	fail "the printer never reported idle" \
		"check the mTLS dial to wss://localhost:$MTLS_HOST_PORT (host mapping frozen in infra/k8s/k3d-cluster.yaml)"
}

# find_socket_owner prints the name of the cloud-server pod holding the
# printer's socket. kube-proxy chose it, so this has to be discovered rather
# than assumed (§6.2). The window starts at the container's creation, so a
# registration line inside it belongs to THIS printer and not to some earlier
# run; the newest line wins if the printer reconnected mid-run (a rollout or a
# pod deletion would do that).
find_socket_owner() {
	local since="${1:-$PRINTER_STARTED_AT}"
	local pod line ts owner="" owner_ts=""
	for pod in $(kc get pods -l app.kubernetes.io/name=cloud-server -o jsonpath='{.items[*].metadata.name}'); do
		line="$(kc logs "$pod" --timestamps --since-time="$since" 2>/dev/null |
			grep -F "printer-link: mailbox ${MAILBOX_ID} registered" | tail -1 || true)"
		[ -n "$line" ] || continue
		ts="${line%% *}"
		if [ -z "$owner_ts" ] || [[ "$ts" > "$owner_ts" ]]; then
			owner="$pod"
			owner_ts="$ts"
		fi
	done
	[ -n "$owner" ] || fail "no cloud-server pod logged a registration for mailbox $MAILBOX_ID since $since" \
		"the printer reported idle, so the socket exists — did the pods restart in between?"
	echo "$owner"
}
