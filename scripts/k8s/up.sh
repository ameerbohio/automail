#!/usr/bin/env bash
# Create the local k3d cluster (Goal K1 / plans/16-kubernetes.md §2).
#
# Everything about the cluster shape lives in infra/k8s/k3d-cluster.yaml; this
# script's job is the checks around it -- the ones whose absence turns into a
# confusing failure ten minutes later:
#
#   * tooling present at the pinned versions (make k8s-tools installs it);
#   * the k3s tag in the config file agrees with versions.env (two files name
#     the same version, so one of them will eventually drift);
#   * the host ports the config publishes are actually free -- k3d's own error
#     for a taken port is a Docker port-bind message several layers down;
#   * the nodes really reach Ready. This host runs cgroup v1 (hybrid), which
#     Kubernetes has had in maintenance mode since 1.31, so node registration
#     is the substrate risk this whole goal exists to settle. If it fails, say
#     so in those terms rather than leaving a half-built cluster around.
#
# Idempotent: re-running against an existing cluster just re-waits for Ready.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
source scripts/k8s/versions.env

CONFIG=infra/k8s/k3d-cluster.yaml
NODE_COUNT=4 # 1 server + 3 agents; keep in step with the config
READY_TIMEOUT="${READY_TIMEOUT:-180}"

for bin in k3d kubectl docker; do
	command -v "$bin" >/dev/null 2>&1 || {
		echo "✗ $bin not found. Run: make k8s-tools" >&2
		exit 1
	}
done
docker info >/dev/null 2>&1 || {
	echo "✗ no Docker daemon -- k3d nodes are containers on it" >&2
	exit 1
}

# One version, two files. Fail loudly rather than booting a cluster whose
# Kubernetes minor is not the one the scripts and docs claim.
cfg_image="$(awk '/^image:/ {print $2; exit}' "$CONFIG")"
if [ "$cfg_image" != "$K3S_IMAGE" ]; then
	echo "✗ k3s image mismatch: $CONFIG has '$cfg_image', versions.env has '$K3S_IMAGE'" >&2
	exit 1
fi

if k3d cluster list -o json 2>/dev/null | grep -q "\"name\":\"$CLUSTER_NAME\""; then
	echo "→ cluster '$CLUSTER_NAME' already exists; waiting for nodes"
else
	# Port pre-flight. Only meaningful before creation -- afterwards the
	# cluster itself holds them.
	for p in "$EDGE_HTTP_PORT" "$EDGE_HTTPS_PORT" "$MTLS_HOST_PORT" "$KUBE_API_PORT"; do
		if ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]$p\$"; then
			echo "✗ host port $p is already in use." >&2
			echo "  The Compose stack owns 8080/8443; the cluster deliberately uses 9080/9443/9843" >&2
			echo "  so both can run at once. Free it, or stop whatever took it, then re-run." >&2
			exit 1
		fi
	done
	echo "→ creating k3d cluster '$CLUSTER_NAME' ($K3S_IMAGE, $NODE_COUNT nodes)"
	k3d cluster create --config "$CONFIG"
fi

echo "→ waiting for $NODE_COUNT nodes to register and reach Ready (${READY_TIMEOUT}s budget)"
deadline=$((SECONDS + READY_TIMEOUT))
while :; do
	registered="$(kubectl get nodes --no-headers 2>/dev/null | wc -l)"
	ready="$(kubectl get nodes --no-headers 2>/dev/null | awk '$2=="Ready"' | wc -l)"
	[ "$ready" -ge "$NODE_COUNT" ] && break
	if [ "$SECONDS" -ge "$deadline" ]; then
		echo "✗ only $ready/$NODE_COUNT nodes Ready ($registered registered) after ${READY_TIMEOUT}s" >&2
		kubectl get nodes -o wide >&2 || true
		echo >&2
		echo "  If nodes never registered, suspect the cgroup hierarchy: this kernel is" >&2
		echo "  cgroup v1 (\`stat -fc %T /sys/fs/cgroup\` = tmpfs). The fix is an OWNER" >&2
		echo "  action -- [boot] in /etc/wsl.conf plus \`wsl --shutdown\` -- not a" >&2
		echo "  workaround in this script. See docs/k8s-host-setup.md." >&2
		exit 1
	fi
	sleep 3
done

kubectl get nodes -o wide
echo "✔ cluster '$CLUSTER_NAME' up: $NODE_COUNT/$NODE_COUNT nodes Ready (context: $(kubectl config current-context))"
echo "  edge https://127.0.0.1:$EDGE_HTTPS_PORT (K4)   printer mTLS 127.0.0.1:$MTLS_HOST_PORT -> nodePort $MTLS_NODE_PORT (K5)"
