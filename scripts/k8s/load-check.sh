#!/usr/bin/env bash
# Goal K7 acceptance: the HorizontalPodAutoscaler under real load.
# Spec: plans/16-kubernetes.md §7, §7.1. Run with `make k8s-load`.
#
# Three phases, one timeline:
#
#   REFERENCE  the HPA is deleted and the tier pinned at ONE replica, then the
#              load is run. This is the number the autoscaled run is compared
#              against. It exists because scripts/load/baseline.json -- the
#              committed Compose baseline -- was measured on a single replica
#              with NO CPU limit, so it is not an apples-to-apples gate for a
#              cluster where every pod has requests and limits (§7.1). The
#              Compose baseline stays context; this is the control.
#   AUTOSCALED the HPA is applied (min 2, max 8, 60% of the CPU request) and the
#              identical load is run again, sampling replica count and measured
#              utilization throughout.
#   COOLDOWN   load stops and the run WAITS OUT the scaleDown stabilization
#              window rather than shortening it, so the recorded scale-down is
#              the production-shaped one (hpa.yaml explains the asymmetry).
#
# Load is `scripts/load/submission.js` unmodified -- the same file `make load`
# runs on Compose -- multiplied by running it in PARALLELISM pods per wave,
# because one k6 pod does not push a 100m CPU request past 60% at two replicas.
#
# Env knobs:
#   PARALLELISM=N        k6 pods per wave (default 3)
#   WAVES=N              waves per phase (default 3)
#   SKIP_REFERENCE=1     skip the single-replica control (a re-run needs it only once)
#   COOLDOWN_TIMEOUT=N   seconds to wait for scale-down (default 600)
#   KEEP_LOAD_OVERLAY=1  leave the load overlay applied (debugging)
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
ROOT="$(pwd)"
source scripts/k8s/versions.env

PARALLELISM="${PARALLELISM:-3}"
WAVES="${WAVES:-3}"
COOLDOWN_TIMEOUT="${COOLDOWN_TIMEOUT:-600}"
LOAD_OVERLAY=infra/k8s/overlays/k3d-load
BASE_OVERLAY=infra/k8s/overlays/k3d-local
HPA_MANIFEST=infra/k8s/base/cloud-server/hpa.yaml
JOB_TEMPLATE=infra/k8s/load/k6-job.yaml
SCRIPT_FILE=scripts/load/submission.js
RESULTS_FILE="$ROOT/infra/k8s/RESULTS.md"
REPORT="$ROOT/scripts/load/report/k8s"
SAMPLER_PID=""

fail() {
	echo "✗ $1" >&2
	[ $# -gt 1 ] && echo "  → $2" >&2
	exit 1
}

kc() { kubectl -n "$NAMESPACE" "$@"; }

cleanup() {
	[ -n "$SAMPLER_PID" ] && kill "$SAMPLER_PID" 2>/dev/null || true
	kc delete job -l app.kubernetes.io/name=k6-load --ignore-not-found >/dev/null 2>&1 || true
	if [ "${KEEP_LOAD_OVERLAY:-}" = "1" ]; then
		echo "==> KEEP_LOAD_OVERLAY=1: the cluster is still signing pre-signed URLs for minio:9000"
		echo "    (the browser flow stays broken until \`make k8s-apply\` restores the edge value)"
		return
	fi
	# ALWAYS restore, including on abort. The load overlay makes cloud-server
	# sign upload URLs for the in-cluster `minio:9000`, which no browser can
	# reach -- leaving it applied would silently break the K4 edge suites and
	# the guest flow with no error anywhere near the cause.
	echo "==> Restoring the browser-facing overlay ($BASE_OVERLAY)"
	bash scripts/k8s/apply.sh >/dev/null 2>&1 ||
		echo "!! restore failed -- run \`make k8s-apply\` before using the edge" >&2
}
trap cleanup EXIT

# --- 0. preflight ---------------------------------------------------------
command -v kubectl >/dev/null 2>&1 || fail "kubectl not found" "make k8s-tools"
command -v k3d >/dev/null 2>&1 || fail "k3d not found" "make k8s-tools"
kubectl cluster-info >/dev/null 2>&1 || fail "no reachable cluster" "make k8s-up"
kc get deploy cloud-server >/dev/null 2>&1 ||
	fail "deployment/cloud-server not found" "make k8s-up && make k8s-images && make k8s-secrets && make k8s-apply"
[ -f "$RESULTS_FILE" ] || fail "infra/k8s/RESULTS.md is missing" \
	"it carries the authored prose; this run only rewrites the block between its K7 markers"

# metrics-server must be HEALTHY, not merely scheduled (§7.1). An HPA whose
# metrics API is unavailable reports <unknown> targets and silently never
# scales, which reads exactly like "the load was too small".
echo "==> Checking the metrics pipeline"
metrics_ok=0
for i in $(seq 1 12); do
	if kc top pods --no-headers >/dev/null 2>&1; then
		metrics_ok=1
		echo "    metrics.k8s.io answering after ${i}s"
		break
	fi
	sleep 1
done
[ "$metrics_ok" = "1" ] || fail "metrics.k8s.io is not serving pod metrics" \
	"kubectl -n kube-system get deploy metrics-server; in k3d it sometimes needs --kubelet-insecure-tls"

# The load generator image, in every node's containerd (there is no registry).
echo "==> Ensuring $K6_IMAGE is in the cluster"
nodes="$(docker ps --filter "name=^k3d-${CLUSTER_NAME}-" --format '{{.Names}}' | grep -Ev 'serverlb|tools' || true)"
[ -n "$nodes" ] || fail "no k3d node containers found for '$CLUSTER_NAME'"
k6_present() {
	local node
	for node in $nodes; do
		docker exec "$node" crictl images 2>/dev/null |
			awk '{print $1":"$2}' | grep -qE "^(docker\.io/)?${K6_IMAGE}$" || return 1
	done
}
if ! k6_present; then
	# See versions.env: the upstream image cannot be imported directly, and the
	# import reports success anyway -- so this verifies rather than trusts.
	echo "    building the local re-tag of $K6_UPSTREAM_IMAGE"
	printf 'FROM %s\n' "$K6_UPSTREAM_IMAGE" | docker build -q -t "$K6_IMAGE" - >/dev/null
	k3d image import -c "$CLUSTER_NAME" "$K6_IMAGE" >/dev/null
	k6_present || fail "$K6_IMAGE is still missing from at least one node's containerd" \
		"\`k3d image import\` exits 0 even when ctr fails -- check its output by hand"
fi
echo "    ✔ present on every node"

mkdir -p "$REPORT"
rm -f "$REPORT"/*.log "$REPORT"/timeline.tsv "$REPORT"/waves.tsv "$REPORT"/meta.tsv

# --- 1. the load profile --------------------------------------------------
# Swaps MINIO_PUBLIC_ENDPOINT to empty so pre-signed upload URLs are signed for
# the in-cluster minio:9000 (the overlay header explains why this is mandatory
# for an in-cluster generator). Restored by the EXIT trap.
echo "==> Applying the load overlay"
OVERLAY="$LOAD_OVERLAY" bash scripts/k8s/apply.sh >/dev/null
echo "    ✔ cloud-server is signing upload URLs for minio:9000"

# --- 2. fixture -----------------------------------------------------------
SEED_BACKEND=kubectl bash scripts/e2e/seed.sh >/dev/null
RECIPIENT="$(kc exec minio-0 -- curl -s "http://cloud-server:8080/recipients?q=Testmann" |
	python3 -c 'import sys,json; print(json.load(sys.stdin)[0]["recipient_id"])')"
[ -n "$RECIPIENT" ] || fail "could not resolve the seeded recipient"
echo "==> Seeded recipient: $RECIPIENT"

# The generator script comes from the canonical file, so a drift between what
# Compose measures and what the cluster measures is impossible by construction.
kc create configmap k6-scripts --from-file="submission.js=$SCRIPT_FILE" \
	--dry-run=client -o yaml | kubectl apply -f - >/dev/null

# --- 3. sampler -----------------------------------------------------------
# One timeline for the whole run: epoch, ready replicas, desired replicas, and
# the utilization the HPA itself is reading (blank while no HPA exists).
sample_loop() {
	# Drop the inherited EXIT trap: this runs as a background subshell, and a
	# copy of cleanup() firing in it would revert the overlay mid-run.
	trap - EXIT
	while true; do
		local ready spec util
		ready="$(kc get deploy cloud-server -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
		spec="$(kc get deploy cloud-server -o jsonpath='{.spec.replicas}' 2>/dev/null || true)"
		util="$(kc get hpa cloud-server -o jsonpath='{.status.currentMetrics[0].resource.current.averageUtilization}' 2>/dev/null || true)"
		printf '%s\t%s\t%s\t%s\n' "$(date +%s)" "${ready:-0}" "${spec:-0}" "${util:-}" >>"$REPORT/timeline.tsv"
		sleep 5
	done
}
sample_loop &
SAMPLER_PID=$!

# --- 4. a wave ------------------------------------------------------------
# Runs PARALLELISM k6 pods of the unmodified submission script, waits for the
# wave, then saves each pod's log (the summary JSON is embedded in it between
# markers -- there is no shared filesystem to write a report to).
run_wave() {
	local phase="$1" wave="$2" job start end
	job="k6-${phase}-w${wave}"
	kc delete job "$job" --ignore-not-found >/dev/null 2>&1 || true
	start="$(date +%s)"
	sed -e "s|__JOB_NAME__|$job|g" \
		-e "s|__PARALLELISM__|$PARALLELISM|g" \
		-e "s|__RECIPIENT_ID__|$RECIPIENT|g" "$JOB_TEMPLATE" |
		kc apply -f - >/dev/null
	if ! kc wait --for=condition=complete --timeout=360s "job/$job" >/dev/null 2>&1; then
		kc describe "job/$job" >&2 || true
		fail "wave $job did not complete" "see the Job events above"
	fi
	end="$(date +%s)"
	local pod
	for pod in $(kc get pods -l "job-name=$job" -o jsonpath='{.items[*].metadata.name}'); do
		kc logs "$pod" >"$REPORT/${phase}-w${wave}-${pod}.log" 2>/dev/null || true
	done
	printf '%s\t%s\t%s\t%s\n' "$phase" "$wave" "$start" "$end" >>"$REPORT/waves.tsv"
	kc delete job "$job" --ignore-not-found >/dev/null 2>&1 || true
	echo "    wave $wave done in $((end - start))s"
}

# --- 5. REFERENCE: one replica, no autoscaler -----------------------------
if [ "${SKIP_REFERENCE:-}" != "1" ]; then
	echo "==> REFERENCE phase: pinning cloud-server at 1 replica (HPA removed)"
	kc delete hpa cloud-server --ignore-not-found >/dev/null
	kc scale deploy cloud-server --replicas=1 >/dev/null
	kc rollout status deploy/cloud-server --timeout=180s >/dev/null
	# maxUnavailable: 0 means the scale-down leaves the survivor serving
	# throughout; the wait above is for the terminating pods' grace period, not
	# for capacity.
	for w in $(seq 1 "$WAVES"); do
		echo "  → reference wave $w/$WAVES ($PARALLELISM k6 pods)"
		run_wave reference "$w"
	done
fi

# --- 6. AUTOSCALED: the HPA drives the replica count ----------------------
echo "==> AUTOSCALED phase: applying the HPA (min 2, max 8, 60% of the CPU request)"
# Applied from the base file rather than the overlay: identical spec, and it
# avoids re-applying the Deployment (whose replicas: 3 would fight the HPA for
# a sync or two -- see the note at the bottom of hpa.yaml).
kc apply -f "$HPA_MANIFEST" >/dev/null
kc rollout status deploy/cloud-server --timeout=180s >/dev/null

# The HPA needs metrics before it can act; until then its target reads
# <unknown> and it will not scale at all. Waiting for a real reading means the
# first wave is measured against a live controller, not a warming one.
echo "==> Waiting for the HPA to report a CPU reading"
for i in $(seq 1 60); do
	cur="$(kc get hpa cloud-server -o jsonpath='{.status.currentMetrics[0].resource.current.averageUtilization}' 2>/dev/null || true)"
	[ -n "$cur" ] && { echo "    HPA reading ${cur}% of target after ${i}s"; break; }
	sleep 2
done
[ -n "${cur:-}" ] || fail "the HPA never reported a CPU metric" \
	"kubectl -n $NAMESPACE describe hpa cloud-server; metrics-server may be unhealthy"

for w in $(seq 1 "$WAVES"); do
	echo "  → autoscaled wave $w/$WAVES ($PARALLELISM k6 pods, replicas now $(kc get deploy cloud-server -o jsonpath='{.status.readyReplicas}'))"
	run_wave autoscaled "$w"
done
LOAD_END="$(date +%s)"

# --- 7. COOLDOWN ----------------------------------------------------------
# Not shortened. hpa.yaml sets scaleDown.stabilizationWindowSeconds to the
# 300s default explicitly, so the controller needs five clear minutes of low
# recommendations before it removes anything; this waits for that to happen for
# real instead of tuning the window down to make the demo quicker.
peak="$(kc get deploy cloud-server -o jsonpath='{.spec.replicas}')"
min_replicas="$(kc get hpa cloud-server -o jsonpath='{.spec.minReplicas}')"
echo "==> COOLDOWN: load stopped at peak ${peak} replicas; waiting out the 300s scaleDown window"
first_drop=""
floor_at=""
for i in $(seq 1 "$((COOLDOWN_TIMEOUT / 5))"); do
	now="$(kc get deploy cloud-server -o jsonpath='{.spec.replicas}')"
	# The HPA can still be scaling UP for a sync or two after the load stops --
	# metrics-server serves a trailing window, so the last elevated sample
	# outlives the last request. Track the peak here as well, or a late
	# scale-up would make the first genuine scale-down invisible to the
	# comparison below.
	[ "$now" -gt "$peak" ] && peak="$now"
	if [ -z "$first_drop" ] && [ "$now" -lt "$peak" ]; then
		first_drop="$(( $(date +%s) - LOAD_END ))"
		echo "    first scale-down after ${first_drop}s (${peak} → ${now})"
	fi
	if [ "$now" -le "$min_replicas" ]; then
		floor_at="$(( $(date +%s) - LOAD_END ))"
		echo "    back to minReplicas after ${floor_at}s"
		break
	fi
	sleep 5
done
[ -n "$floor_at" ] || echo "!! did not reach minReplicas within ${COOLDOWN_TIMEOUT}s (at $(kc get deploy cloud-server -o jsonpath='{.spec.replicas}') replicas)"

kill "$SAMPLER_PID" 2>/dev/null || true
SAMPLER_PID=""

# --- 8. report ------------------------------------------------------------
{
	printf 'run_at\t%s\n' "$(date -Is)"
	printf 'k3s\t%s\n' "$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion}')"
	printf 'parallelism\t%s\n' "$PARALLELISM"
	printf 'waves\t%s\n' "$WAVES"
	printf 'cpu_request\t%s\n' "$(kc get deploy cloud-server -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}')"
	printf 'cpu_limit\t%s\n' "$(kc get deploy cloud-server -o jsonpath='{.spec.template.spec.containers[0].resources.limits.cpu}')"
	printf 'target_utilization\t%s\n' "$(kc get hpa cloud-server -o jsonpath='{.spec.metrics[0].resource.target.averageUtilization}')"
	printf 'min_replicas\t%s\n' "$(kc get hpa cloud-server -o jsonpath='{.spec.minReplicas}')"
	printf 'max_replicas\t%s\n' "$(kc get hpa cloud-server -o jsonpath='{.spec.maxReplicas}')"
	printf 'peak_replicas\t%s\n' "$peak"
	printf 'load_end\t%s\n' "$LOAD_END"
	printf 'first_drop_seconds\t%s\n' "${first_drop:-}"
	printf 'floor_seconds\t%s\n' "${floor_at:-}"
} >"$REPORT/meta.tsv"

echo "==> Building the report"
python3 scripts/load/k8s-report.py "$REPORT" "$RESULTS_FILE"
