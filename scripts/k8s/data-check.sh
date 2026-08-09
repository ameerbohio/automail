#!/usr/bin/env bash
# Prove the data tier is real state, not a pod that happens to run a database
# (Goal K2 acceptance / plans/16-kubernetes.md §5).
#
# Three things get checked, in order:
#
#  1. THE SCHEMA APPLIED. `\dt` inside postgres-0 lists the tables from
#     services/cloud/db/schema.sql -- i.e. the ConfigMap really was read by
#     /docker-entrypoint-initdb.d on first init, the behaviour Compose relies
#     on and this port had to preserve.
#
#  2. DATA SURVIVES POD DELETION. Write a marker row, `kubectl delete pod`,
#     wait for the StatefulSet to bring postgres-0 back, read the marker.
#
#  3. WHICH NODE IT CAME BACK ON, recorded rather than assumed. This is the
#     honest half: the data survives because `local-path` provisioned a
#     node-local directory and pinned the PV to that node with a nodeAffinity,
#     so the replacement pod is *scheduled back to the same node*. It is not
#     network-attached storage and it does not survive losing the node. The
#     same property is why the overlay pins the data tier to the k3d server
#     node -- so K6 can drain an agent without stranding Postgres in Pending.
#
# Read-mostly: the marker lives in its own table, which is dropped at the end.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
source scripts/k8s/versions.env

MARKER="k2-data-check-$(date -u +%Y%m%dT%H%M%SZ)"
POD=postgres-0

fail() {
	echo "✗ $1" >&2
	[ $# -gt 1 ] && echo "  → $2" >&2
	exit 1
}
psql_in_pod() { # $1 = SQL
	kubectl -n "$NAMESPACE" exec "$POD" -- \
		sh -c 'psql -qtAX -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "$0"' "$1"
}
node_of() { kubectl -n "$NAMESPACE" get pod "$POD" -o jsonpath='{.spec.nodeName}'; }

command -v kubectl >/dev/null 2>&1 || fail "kubectl not found" "make k8s-tools"
kubectl -n "$NAMESPACE" get pod "$POD" >/dev/null 2>&1 || fail "$POD not found in $NAMESPACE" "make k8s-apply"

# --- 1. schema ------------------------------------------------------------
tables="$(psql_in_pod "SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY 1" | tr -d '\r')"
count="$(printf '%s\n' "$tables" | grep -c . || true)"
[ "$count" -gt 0 ] || fail "no tables in the public schema" "the initdb ConfigMap did not run; PGDATA may predate it (RESET_DATA=1 ALLOW_DESTRUCTIVE=1 make k8s-apply)"
for t in buildings mailboxes senders jobs audit_events; do
	grep -qx "$t" <<<"$tables" || fail "expected table '$t' missing" "schema.sql did not apply cleanly"
done
echo "✔ schema applied from the ConfigMap: $count tables ($(tr '\n' ' ' <<<"$tables"))"

# --- 2. write a marker, kill the pod --------------------------------------
before="$(node_of)"
pvc="$(kubectl -n "$NAMESPACE" get pod "$POD" -o jsonpath='{.spec.volumes[?(@.persistentVolumeClaim)].persistentVolumeClaim.claimName}')"
psql_in_pod "CREATE TABLE IF NOT EXISTS _k8s_data_check (marker TEXT PRIMARY KEY, at TIMESTAMPTZ NOT NULL DEFAULT now())" >/dev/null
psql_in_pod "INSERT INTO _k8s_data_check (marker) VALUES ('$MARKER')" >/dev/null
echo "✔ marker written on node $before (pvc $pvc): $MARKER"

echo "→ deleting pod $POD"
kubectl -n "$NAMESPACE" delete pod "$POD" --wait=true >/dev/null
# `kubectl wait` fails immediately with NotFound if it runs in the gap between
# the delete completing and the StatefulSet controller recreating the pod --
# which is exactly where we are. Wait for the object to exist first, then for
# it to be Ready.
for _ in $(seq 60); do
	kubectl -n "$NAMESPACE" get pod "$POD" >/dev/null 2>&1 && break
	sleep 1
done
kubectl -n "$NAMESPACE" wait --for=condition=Ready "pod/$POD" --timeout=120s >/dev/null ||
	fail "$POD did not come back Ready within 120s" "kubectl -n $NAMESPACE describe pod $POD (a Pending pod means its local-path PV is on an unavailable node)"

# --- 3. read it back, and record where from -------------------------------
after="$(node_of)"
got="$(psql_in_pod "SELECT marker FROM _k8s_data_check WHERE marker='$MARKER'" | tr -d '\r\n ')"
[ "$got" = "$MARKER" ] || fail "marker did not survive the restart (read '$got')" "the PVC was not reattached"
psql_in_pod "DROP TABLE _k8s_data_check" >/dev/null

echo "✔ marker survived pod deletion: $got"
echo "✔ pod rescheduled onto node: $after (was $before)"
if [ "$before" = "$after" ]; then
	echo "  ↳ same node, as local-path requires: the PV carries a nodeAffinity to it."
	echo "    Storage-class property, not a Kubernetes guarantee -- see plans/16-kubernetes.md §2.1."
else
	echo "⚠ different node -- unexpected with local-path; the PV should have pinned it." >&2
	echo "  Check the StorageClass: $(kubectl -n "$NAMESPACE" get pvc "$pvc" -o jsonpath='{.spec.storageClassName}')" >&2
fi
