#!/usr/bin/env bash
# Delete the local k3d cluster and PROVE nothing is left behind (Goal K1).
#
# `k3d cluster delete` is the easy half. The acceptance is the second half:
# no k3d-* containers, networks or volumes survive. That matters on this host
# because the image volume (`k3d-<cluster>-images`) and the per-cluster bridge
# network are separate Docker objects from the node containers, and a leaked
# one silently eats disk or collides with the next `k8s-up`.
#
# Runs clean when there is no cluster (make it safe to call from teardown
# paths and CI).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
source scripts/k8s/versions.env

command -v k3d >/dev/null 2>&1 || {
	echo "⚠ k3d not installed -- nothing to tear down"
	exit 0
}
docker info >/dev/null 2>&1 || {
	echo "⚠ no Docker daemon -- nothing to tear down"
	exit 0
}

if k3d cluster list -o json 2>/dev/null | grep -q "\"name\":\"$CLUSTER_NAME\""; then
	k3d cluster delete "$CLUSTER_NAME"
else
	echo "→ no cluster '$CLUSTER_NAME'"
fi

leaked=0
report() { # $1=kind  $2=leftovers
	if [ -n "$2" ]; then
		echo "✗ leftover k3d $1:" >&2
		echo "$2" | sed 's/^/    /' >&2
		leaked=1
	fi
}
report containers "$(docker ps -a --filter 'name=^k3d-' --format '{{.Names}}')"
report networks "$(docker network ls --filter 'name=^k3d-' --format '{{.Name}}')"
report volumes "$(docker volume ls --filter 'name=^k3d-' --format '{{.Name}}')"

if [ "$leaked" -ne 0 ]; then
	echo "  Remove them with: docker rm -f / docker network rm / docker volume rm" >&2
	exit 1
fi
echo "✔ cluster '$CLUSTER_NAME' gone: no k3d containers, networks or volumes remain"
