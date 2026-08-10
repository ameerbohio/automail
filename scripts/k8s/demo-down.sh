#!/usr/bin/env bash
# Tear the public demo down: kill the tunnel, stop the printer, and put the
# edge back on the local overlay. `make k8s-demo-down`.
#
# The CLUSTER survives. Only the internet exposure and the demo's edge routing
# go away, so this is the opposite of `make k8s-down` -- nothing is deleted and
# no PVC is touched. Re-applying k3d-local restores the three Host()-matched
# HTTPS routes and the :9443 origin values.
#
# KEEP_EDGE=1 leaves the demo routing in place and only removes the tunnel,
# for when the next thing you do is re-run `make k8s-demo`.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

export PRINT="${PRINT:-}"
# shellcheck source=scripts/k8s/lib-printer.sh
source scripts/k8s/lib-printer.sh

echo "==> Removing the Cloudflare tunnel"
docker rm -f automail-k8s-tunnel >/dev/null 2>&1 || true

echo "==> Stopping the printer"
printer_down

if [ "${KEEP_EDGE:-}" = "1" ]; then
	echo "==> KEEP_EDGE=1: leaving the demo overlay applied"
else
	echo "==> Restoring the local edge (k3d-local)"
	# The demo's IngressRoutes are named demo-*, so re-applying k3d-local
	# recreates the base routes but does NOT remove them -- `apply -k` only
	# owns what it declares. Delete them by name.
	kc delete ingressroute demo-portal demo-minio --ignore-not-found >/dev/null
	kubectl apply -k infra/k8s/overlays/k3d-local >/dev/null
	# `kubectl set env` wrote literal values over the ConfigMap references in
	# demo-up.sh step 4. `apply -k` will not undo that on its own: an explicit
	# env entry the applied spec no longer mentions is still removed by apply's
	# three-way merge ONLY if the last-applied annotation contained it -- and
	# `set env` updated that annotation. Re-applying therefore does restore the
	# configMapKeyRef form, but assert it rather than assume it.
	kc rollout status deploy/cloud-server --timeout=180s >/dev/null
	if kc get deploy cloud-server -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="MINIO_PUBLIC_ENDPOINT")].value}' |
		grep -q trycloudflare; then
		echo "⚠ cloud-server still has a literal tunnel endpoint; forcing it back" >&2
		kc set env deployment/cloud-server MINIO_PUBLIC_ENDPOINT- MINIO_PUBLIC_SECURE- >/dev/null
		kubectl apply -k infra/k8s/overlays/k3d-local >/dev/null
		kc rollout status deploy/cloud-server --timeout=180s >/dev/null
	fi
fi

echo "✔ demo down. The cluster is still up: kubectl -n $NAMESPACE get pods"
