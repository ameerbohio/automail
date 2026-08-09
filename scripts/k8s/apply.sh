#!/usr/bin/env bash
# Apply the k3d overlay and wait for it to actually come up (Goal K2).
#
# `kubectl apply -k` alone is not enough, for two reasons this script exists to
# handle:
#
#  1. THE SCHEMA CONFIGMAP. Postgres is initialised from
#     services/cloud/db/schema.sql -- the same file docker-compose.yml mounts
#     into /docker-entrypoint-initdb.d. Kustomize refuses to read files above
#     its own root, so a configMapGenerator cannot reach it, and copying the
#     schema under infra/k8s/ would create a second source of truth that
#     silently drifts from the Compose path (schema drift between two deploy
#     targets is the kind of bug that only shows up as a runtime SQL error).
#     So the ConfigMap is created here, from the canonical file.
#
#     No name-suffix hash on it, deliberately: /docker-entrypoint-initdb.d runs
#     ONLY on first init of an empty PGDATA, so rolling the pod on a schema
#     edit would imply an effect it does not have. Applying a new schema means
#     wiping the volume -- RESET_DATA=1, guarded below.
#
#  2. SECRET PRECONDITIONS. Every workload mounts Secrets that this repo
#     cannot contain. If they are absent the pods sit in
#     CreateContainerConfigError, which reads like a manifest bug. Check first
#     and say `make k8s-secrets` instead.
#
# Env knobs:
#   RESET_DATA=1 + ALLOW_DESTRUCTIVE=1   delete the data-tier PVCs first (the
#                                        only way a schema change takes effect)
#   OVERLAY=<path>                       apply a different overlay
#   WAIT_TIMEOUT=<seconds>               rollout wait budget (default 180)
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
source scripts/k8s/versions.env

OVERLAY="${OVERLAY:-infra/k8s/overlays/k3d-local}"
SCHEMA_FILE=services/cloud/db/schema.sql
WAIT_TIMEOUT="${WAIT_TIMEOUT:-180}"

fail() {
	echo "✗ $1" >&2
	[ $# -gt 1 ] && echo "  → $2" >&2
	exit 1
}

command -v kubectl >/dev/null 2>&1 || fail "kubectl not found" "make k8s-tools"
kubectl cluster-info >/dev/null 2>&1 || fail "no reachable cluster" "make k8s-up"
kubectl get namespace "$NAMESPACE" >/dev/null 2>&1 ||
	fail "namespace $NAMESPACE does not exist" "make k8s-secrets (it creates the namespace and the Secrets)"
for s in "$CRED_SECRET" "$CERT_SECRET" "$EDGE_SECRET"; do
	kubectl -n "$NAMESPACE" get secret "$s" >/dev/null 2>&1 ||
		fail "secret/$s missing in $NAMESPACE" "make k8s-secrets"
done
echo "✔ preconditions: namespace + 3 secrets present"

# --- destructive path, opt-in twice ---------------------------------------
if [ "${RESET_DATA:-}" = "1" ]; then
	[ "${ALLOW_DESTRUCTIVE:-}" = "1" ] ||
		fail "RESET_DATA=1 destroys the data-tier PVCs (every job, sender and mailbox row)" \
			"re-run with ALLOW_DESTRUCTIVE=1 if that data is disposable"
	echo "→ RESET_DATA: deleting data-tier StatefulSets and their PVCs"
	kubectl -n "$NAMESPACE" delete statefulset postgres redis minio --ignore-not-found
	kubectl -n "$NAMESPACE" delete pvc -l app.kubernetes.io/part-of=automail --ignore-not-found
	# volumeClaimTemplates PVCs are named data-<sts>-<ordinal> and are NOT
	# labelled by the StatefulSet, so the label selector above misses them.
	kubectl -n "$NAMESPACE" delete pvc data-postgres-0 data-redis-0 data-minio-0 --ignore-not-found
fi

# --- schema ConfigMap ------------------------------------------------------
[ -s "$SCHEMA_FILE" ] || fail "$SCHEMA_FILE not found" "this is the canonical schema both deploy targets use"
kubectl -n "$NAMESPACE" create configmap "$SCHEMA_CONFIGMAP" \
	--from-file="01-schema.sql=$SCHEMA_FILE" --dry-run=client -o yaml |
	kubectl apply -f - >/dev/null
echo "✔ configmap/$SCHEMA_CONFIGMAP from $SCHEMA_FILE ($(wc -c <"$SCHEMA_FILE") bytes)"

# --- cluster-scoped policy, applied outside the overlay --------------------
# The default TLSOption governs every Traefik router in the cluster, not just
# Automail's, so it lives outside the namespaced overlay (see the file's
# header). Applied only when the CRD exists, so a cluster without Traefik --
# or a dry-run of the app manifests alone -- is not blocked by it.
if kubectl get crd tlsoptions.traefik.io >/dev/null 2>&1; then
	kubectl apply -f infra/k8s/cluster/default-tlsoption.yaml >/dev/null
	echo "✔ cluster default TLSOption (sniStrict) applied"
else
	echo "⚠ traefik CRDs absent — skipping the cluster default TLSOption"
fi

# --- the manifests ---------------------------------------------------------
kubectl apply -k "$OVERLAY"

# --- wait for it to be real ------------------------------------------------
# `rollout status` on a StatefulSet returns when the replica is Ready, i.e.
# after the probes pass -- not merely when the pod is scheduled.
for sts in $(kubectl -n "$NAMESPACE" get statefulset -o name); do
	echo "→ waiting for $sts"
	kubectl -n "$NAMESPACE" rollout status "$sts" --timeout="${WAIT_TIMEOUT}s"
done
for dep in $(kubectl -n "$NAMESPACE" get deployment -o name 2>/dev/null); do
	echo "→ waiting for $dep"
	kubectl -n "$NAMESPACE" rollout status "$dep" --timeout="${WAIT_TIMEOUT}s"
done

echo
kubectl -n "$NAMESPACE" get pods -o wide
echo
# Which node a data-tier pod landed on is not trivia: local-path pins its PV
# there for the pod's whole life (see the overlay's nodeSelector comment).
kubectl -n "$NAMESPACE" get pvc
echo "✔ overlay $OVERLAY applied and Ready"
