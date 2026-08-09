#!/usr/bin/env bash
# Build the two service images and put them inside the cluster (Goal K1).
#
# There is no registry here. `k3d image import` copies a locally built image
# straight into every node's containerd, so `docker build` stays the one build
# path -- the same Dockerfiles Compose uses, no push, no credentials.
#
# Two traps this script exists to keep you out of:
#
#   * NEVER tag these :latest. Kubernetes forces imagePullPolicy=Always on the
#     :latest tag, and a cluster with no registry answers that with
#     ImagePullBackOff. Hence the explicit :dev tags in versions.env, paired
#     with imagePullPolicy: IfNotPresent in the manifests (K3/K4).
#   * `k3d image import` keys on image *ID*. Rebuilding under the same tag
#     changes the ID, so the cluster keeps serving the old layers until the
#     image is re-imported AND the pods are restarted -- otherwise you are
#     debugging a stale binary. This script does both: import, then roll any
#     Deployment already running that tag.
#
# Env knobs:
#   ONLY=cloud|portal   build/import just one image
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
source scripts/k8s/versions.env

ONLY="${ONLY:-}"

command -v k3d >/dev/null 2>&1 || {
	echo "✗ k3d not found. Run: make k8s-tools" >&2
	exit 1
}
k3d cluster list -o json 2>/dev/null | grep -q "\"name\":\"$CLUSTER_NAME\"" || {
	echo "✗ cluster '$CLUSTER_NAME' does not exist. Run: make k8s-up" >&2
	exit 1
}

images=()
[ "$ONLY" = "portal" ] || images+=("$CLOUD_IMAGE|services/cloud")
[ "$ONLY" = "cloud" ] || images+=("$PORTAL_IMAGE|services/portal")

for spec in "${images[@]}"; do
	tag="${spec%|*}"
	ctx="${spec##*|}"
	echo "→ docker build $tag ($ctx)"
	docker build -q -t "$tag" "$ctx" >/dev/null
done

echo "→ k3d image import into cluster '$CLUSTER_NAME'"
k3d image import -c "$CLUSTER_NAME" "${images[@]%|*}"

# Verify from the cluster's own view, not Docker's: containerd on each node is
# what kubelet pulls from, and that is the thing the import had to reach.
nodes="$(docker ps --filter "name=^k3d-${CLUSTER_NAME}-" --format '{{.Names}}' | grep -Ev 'serverlb|tools' || true)"
[ -n "$nodes" ] || {
	echo "✗ no k3d node containers found for '$CLUSTER_NAME'" >&2
	exit 1
}
for node in $nodes; do
	for spec in "${images[@]}"; do
		tag="${spec%|*}"
		# containerd normalises short names, so the tag comes back fully
		# qualified as docker.io/automail/... even though nothing was pulled
		# from Docker Hub. Match with the registry prefix optional.
		if ! docker exec "$node" crictl images 2>/dev/null |
			awk '{print $1":"$2}' | grep -qE "^(docker\.io/)?${tag}$"; then
			echo "✗ $tag missing from containerd on $node" >&2
			exit 1
		fi
	done
	echo "  ✔ $node has: ${images[*]%|*}"
done

# Re-import without a restart is the stale-binary trap; roll anything already
# running these tags. No-ops before K3, when no Deployment exists yet.
if kubectl get deploy -A >/dev/null 2>&1; then
	for spec in "${images[@]}"; do
		tag="${spec%|*}"
		kubectl get deploy -A \
			-o jsonpath="{range .items[?(@.spec.template.spec.containers[0].image=='$tag')]}{.metadata.namespace}/{.metadata.name}{'\n'}{end}" 2>/dev/null |
			while read -r d; do
				[ -n "$d" ] || continue
				echo "→ rollout restart ${d} (new image ID for $tag)"
				kubectl -n "${d%%/*}" rollout restart "deploy/${d##*/}"
			done
	done
fi

echo "✔ images in cluster: ${images[*]%|*}"
