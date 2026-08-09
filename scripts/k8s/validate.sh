#!/usr/bin/env bash
# Docker-free manifest validation (Goal K1 / plans/16-kubernetes.md §2.1).
#
# This is the gate that belongs in `make ci`: it needs kubectl and nothing
# else -- no cluster, no daemon -- so a manifest typo fails on the machine that
# wrote it instead of during a cluster bring-up. It checks three things:
#
#   1. the k3s image pin agrees between infra/k8s/k3d-cluster.yaml and
#      scripts/k8s/versions.env (two files, one version);
#   2. no manifest tags an image `:latest` -- registry-less k3d turns that into
#      ImagePullBackOff, and the failure surfaces far from its cause;
#   3. every Kustomize overlay builds, and its output survives
#      `kubectl apply --dry-run=client` (schema/structure, not admission).
#
# Client-side dry-run only: server-side validation would need a cluster, which
# is exactly what this target promises not to need.
#
# Skips (exit 0) when kubectl is absent, so `make ci` stays runnable on a
# machine that never touches Kubernetes.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
source scripts/k8s/versions.env

CONFIG=infra/k8s/k3d-cluster.yaml

if ! command -v kubectl >/dev/null 2>&1; then
	echo "⚠ k8s-validate skipped: kubectl not installed (make k8s-tools)"
	exit 0
fi

cfg_image="$(awk '/^image:/ {print $2; exit}' "$CONFIG")"
if [ "$cfg_image" != "$K3S_IMAGE" ]; then
	echo "✗ k3s image pin mismatch: $CONFIG='$cfg_image' versions.env='$K3S_IMAGE'" >&2
	exit 1
fi
echo "✔ k3s image pinned consistently: $K3S_IMAGE"

overlays=()
while IFS= read -r d; do overlays+=("$d"); done < <(
	find infra/k8s/overlays -mindepth 1 -maxdepth 1 -type d 2>/dev/null | sort
)
if [ "${#overlays[@]}" -eq 0 ]; then
	echo "⚠ no overlays under infra/k8s/overlays -- nothing to build"
	exit 0
fi

for overlay in "${overlays[@]}"; do
	rendered="$(kubectl kustomize "$overlay")"
	if [ -z "${rendered//[[:space:]]/}" ]; then
		# Expected until K2 populates the base; an empty build is still proof
		# the kustomization files parse.
		echo "✔ $overlay builds (no resources yet -- populated from Goal K2)"
		continue
	fi
	if grep -qE '^\s*image:.*:latest\s*$' <<<"$rendered"; then
		echo "✗ $overlay renders an image tagged :latest (ImagePullBackOff in a registry-less k3d)" >&2
		exit 1
	fi
	kubectl apply --dry-run=client -f - <<<"$rendered" >/dev/null
	echo "✔ $overlay builds and passes client dry-run ($(grep -c '^kind:' <<<"$rendered") objects)"
done
