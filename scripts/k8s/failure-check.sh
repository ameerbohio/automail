#!/usr/bin/env bash
# Goal K6 acceptance: the failure and rollout behaviour that Compose cannot
# express. Spec: plans/16-kubernetes.md §5, §8. Run with `make k8s-failure`.
#
# Three scenarios, all driven by e2e/k8s_failure_test.go (build tag `k8sfail`):
#
#   1. delete the pod holding the printer's socket — the printer redials the
#      NodePort and lands on a SURVIVING pod, and jobs submitted while no
#      socket exists anywhere still reach `delivered` exactly once.
#   2. `kubectl drain` an agent node against the PodDisruptionBudget, plus a
#      direct eviction the budget must refuse.
#   3. `kubectl rollout restart` under continuous traffic — zero non-2xx.
#
# WHY THE GO DRIVER RUNS kubectl HERE, unlike scripts/k8s/e2e.sh. In K5 the
# cluster manipulation all happened before the HTTP driver started, so the
# script could own it. Here the kill, the drain and the rollout have to be
# interleaved with live traffic and with submissions timed against a window
# only the driver can observe (the moment the printer's socket disappears from
# Redis). Splitting that across a pipe would be worse than letting the driver
# shell out. The script still owns the one-time bring-up.
#
# Env knobs:
#   KEEP_PRINTER=1   leave the printer container up afterwards (debugging)
#   NO_BUILD=1       skip the printer image rebuild
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
# shellcheck source=scripts/k8s/lib-printer.sh
source scripts/k8s/lib-printer.sh

RESULTS_FILE="$ROOT/infra/k8s/RESULTS.md"

cleanup() {
	# Always uncordon, even though the driver uncordons in its own defer: a run
	# killed mid-drain (Ctrl-C, `go test` timeout) never reaches that defer, and
	# a node left unschedulable surfaces an hour later as a `Pending` pod with
	# no obvious cause.
	for node in $(kubectl get nodes -o jsonpath='{range .items[?(@.spec.unschedulable==true)]}{.metadata.name}{"\n"}{end}' 2>/dev/null); do
		echo "==> uncordoning leftover node $node"
		kubectl uncordon "$node" >/dev/null 2>&1 || true
	done
	if [ "${KEEP_PRINTER:-}" = "1" ]; then
		echo "==> KEEP_PRINTER=1: leaving $PRINTER_CONTAINER up"
		return
	fi
	printer_down
}
trap cleanup EXIT

# --- 0. preflight ---------------------------------------------------------
preflight_cluster
[ -f "$RESULTS_FILE" ] || fail "infra/k8s/RESULTS.md is missing" \
	"it carries the prose; the driver only rewrites the block between its BEGIN/END markers"

# The PDB scenario is only meaningful against the manifest's 3 replicas. If
# Goal K7's HPA has scaled the Deployment down to 2, minAvailable: 2 blocks
# every eviction — which is the PDB working, not a bug, but it is not what
# this run set out to measure. Say so up front instead of failing obscurely
# inside the drain.
replicas="$(kc get deploy cloud-server -o jsonpath='{.spec.replicas}')"
[ "$replicas" -ge 3 ] || fail "cloud-server is at $replicas replicas, need >= 3" \
	"a PDB of minAvailable: 2 refuses every eviction below 3 — scale back up (make k8s-apply) or record the HPA state deliberately"

# --- 1. object storage must be reachable from outside the cluster ---------
resolve_minio_host

# --- 2. seed the cluster's Postgres ---------------------------------------
SEED_BACKEND=kubectl bash scripts/e2e/seed.sh

# --- 3. the printer, outside the cluster ----------------------------------
start_printer
wait_printer_idle
owner="$(find_socket_owner)"
echo "✔ printer socket owner: $owner"

# --- 4. credentials for the audit-ledger assertion ------------------------
# "Exactly once" is read from the append-only audit_events table, the same
# ledger the Compose chaos suite uses, so the driver needs psql credentials.
PG_USER="$(grep '^POSTGRES_USER=' .env | cut -d= -f2-)"
PG_DB="$(grep '^POSTGRES_DB=' .env | cut -d= -f2-)"
PG_PASSWORD="$(grep '^POSTGRES_PASSWORD=' .env | cut -d= -f2-)"

# --- 5. drive it ----------------------------------------------------------
echo "==> Running the Kubernetes failure/rollout driver"
cd e2e
E2E_REPO_ROOT="$ROOT" \
	E2E_PRINTER_CONTAINER="$PRINTER_CONTAINER" \
	E2E_PG_USER="$PG_USER" \
	E2E_PG_DB="$PG_DB" \
	E2E_PG_PASSWORD="$PG_PASSWORD" \
	K8S_EDGE_HTTPS_PORT="$EDGE_HTTPS_PORT" \
	K8S_NAMESPACE="$NAMESPACE" \
	K8S_MAILBOX_ID="$MAILBOX_ID" \
	K8S_OWNER_POD="$owner" \
	K8S_PRINTER_STARTED_AT="$PRINTER_STARTED_AT" \
	K8S_RESULTS_FILE="$RESULTS_FILE" \
	go test -tags k8sfail -count=1 -v -timeout 20m ./...
