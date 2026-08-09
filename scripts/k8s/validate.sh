#!/usr/bin/env bash
# Docker-free, cluster-free manifest validation (Goal K1 / plans/16-kubernetes.md §2.1).
#
# This is the gate that belongs in `make ci`: it needs no cluster and no Docker
# daemon, so a manifest typo fails on the machine that wrote it instead of
# during a cluster bring-up. It checks four things:
#
#   1. the k3s image pin agrees between infra/k8s/k3d-cluster.yaml and
#      scripts/k8s/versions.env (two files, one version);
#   2. every Kustomize overlay builds;
#   3. no manifest tags an image `:latest` -- registry-less k3d turns that into
#      ImagePullBackOff, and the failure surfaces far from its cause;
#   4. every rendered object validates against the Kubernetes JSON schema for
#      the pinned server version (kubeconform, offline).
#
# WHY NOT `kubectl apply --dry-run=client`, WHICH THIS SCRIPT USED TO DO:
# because it is not actually client-side. It needs the API server twice -- once
# to download the OpenAPI schema for validation, and again for discovery, to map
# a Kind onto a resource -- so without a reachable cluster it fails with
#
#     failed to download openapi: ... dial tcp [::1]:8080: connect: connection refused
#     unable to recognize "STDIN": Get "http://localhost:8080/api?timeout=32s"
#
# and `--validate=false` silences only the first of the two. That is why this
# job passed for months on a laptop with a live k3d cluster in its kubeconfig
# and had never once passed in CI: the check was quietly reading the cluster it
# claimed not to need. kubeconform validates against downloaded JSON schemas
# instead, so it is genuinely cluster-free -- and strictly stronger, since it
# catches a misspelled field, which a dry-run apply never did.
#
# Env knobs:
#   REQUIRE_KUBECONFORM=1   fail (don't warn) when kubeconform is absent. Set in
#                           CI, where a silently skipped check is how the bug
#                           above survived in the first place.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
source scripts/k8s/versions.env

CONFIG=infra/k8s/k3d-cluster.yaml

if ! command -v kubectl >/dev/null 2>&1; then
	echo "⚠ k8s-validate skipped: kubectl not installed (make k8s-tools)"
	exit 0
fi

# --- 1. one version, two files ---------------------------------------------
cfg_image="$(awk '/^image:/ {print $2; exit}' "$CONFIG")"
if [ "$cfg_image" != "$K3S_IMAGE" ]; then
	echo "✗ k3s image pin mismatch: $CONFIG='$cfg_image' versions.env='$K3S_IMAGE'" >&2
	exit 1
fi
echo "✔ k3s image pinned consistently: $K3S_IMAGE"

# --- 2. the schema validator -----------------------------------------------
# Absent locally is a warning; absent in CI is a failure. An optional check that
# silently no-ops is indistinguishable from a passing one.
KUBECONFORM=""
if command -v kubeconform >/dev/null 2>&1; then
	KUBECONFORM="kubeconform"
elif [ "${REQUIRE_KUBECONFORM:-}" = "1" ]; then
	echo "✗ kubeconform not found and REQUIRE_KUBECONFORM=1" >&2
	echo "  → run: bash scripts/k8s/tools.sh" >&2
	exit 1
else
	echo "⚠ kubeconform not installed -- schema validation SKIPPED (make k8s-tools)"
	echo "  structure and pins are still checked below, but a misspelled field will not be caught"
fi

overlays=()
while IFS= read -r d; do overlays+=("$d"); done < <(
	find infra/k8s/overlays -mindepth 1 -maxdepth 1 -type d 2>/dev/null | sort
)
if [ "${#overlays[@]}" -eq 0 ]; then
	echo "⚠ no overlays under infra/k8s/overlays -- nothing to build"
	exit 0
fi

for overlay in "${overlays[@]}"; do
	# `kubectl kustomize` is pure client-side rendering -- no API server, unlike
	# `kubectl apply`. This step alone catches the most common breakage: a patch
	# whose target matches nothing, a missing resource file, a bad merge.
	rendered="$(kubectl kustomize "$overlay")"
	if [ -z "${rendered//[[:space:]]/}" ]; then
		echo "✔ $overlay builds (no resources yet)"
		continue
	fi

	# --- 3. the :latest trap ---------------------------------------------
	if grep -qE '^\s*image:.*:latest\s*$' <<<"$rendered"; then
		echo "✗ $overlay renders an image tagged :latest (ImagePullBackOff in a registry-less k3d)" >&2
		exit 1
	fi

	objects="$(grep -c '^kind:' <<<"$rendered")"
	if [ -z "$KUBECONFORM" ]; then
		echo "✔ $overlay builds ($objects objects, schema unchecked)"
		continue
	fi

	# --- 4. schema validation, offline -----------------------------------
	# -strict rejects unknown fields (the whole point -- `replicaz: 3` is
	# valid YAML and a broken Deployment). -ignore-missing-schemas skips the
	# Traefik CRDs, which have no published JSON schema; the summary line
	# below reports how many were skipped rather than hiding it.
	summary="$($KUBECONFORM -strict -summary -ignore-missing-schemas \
		-kubernetes-version "$KUBECONFORM_K8S_VERSION" - <<<"$rendered" 2>&1)" || {
		echo "✗ $overlay failed schema validation:" >&2
		echo "$summary" >&2
		exit 1
	}
	echo "✔ $overlay builds and validates ($objects objects) — ${summary#Summary: }"
done
