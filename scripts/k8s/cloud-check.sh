#!/usr/bin/env bash
# Goal K3 acceptance for the cloud-server Deployment
# (plans/16-kubernetes.md §3, §5). Four claims, each measured, none assumed:
#
#   1. THREE PODS ON AT LEAST TWO NODES -- the preferred podAntiAffinity did
#      something. Preferred, so this is a scheduling outcome, not a guarantee.
#   2. THE SERVICE LOAD BALANCES -- repeated in-cluster requests come back with
#      at least two distinct `X-Automail-Node` values, and every value is a
#      real pod name (NODE_ID from the downward API, not a random hostname).
#   3. THE CONSUMER GROUP MATCHES THE POD COUNT, AND STILL DOES AFTER A ROLLING
#      RESTART. This is the Goal K0 regression guard. Pod names change on every
#      rollout, so the Redis consumer name changes with them; without K0's
#      XGROUP DELCONSUMER this Deployment would leak three consumers per
#      restart, forever.
#   4. maxUnavailable: 0 IS HONOURED -- readyReplicas is sampled throughout the
#      restart and never drops below the desired count.
#
# Requests are driven from inside the cluster (kubectl exec into minio-0, which
# ships curl) rather than through the ingress: K4 owns the edge, and going
# through it here would put Traefik and the guest rate limit in the measured
# path.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
source scripts/k8s/versions.env

DEPLOY=cloud-server
CLIENT_POD=minio-0 # neutral in-cluster HTTP client (has curl)
REQUESTS="${REQUESTS:-12}"

fail() {
	echo "✗ $1" >&2
	[ $# -gt 1 ] && echo "  → $2" >&2
	exit 1
}
kc() { kubectl -n "$NAMESPACE" "$@"; }
pod_names() { kc get pods -l app.kubernetes.io/name="$DEPLOY" -o jsonpath='{.items[*].metadata.name}'; }
consumers() { # names in the dispatchers group, one per line
	kc exec redis-0 -- redis-cli XINFO CONSUMERS jobs:pending dispatchers 2>/dev/null |
		awk '/^name$/{getline; print}'
}

command -v kubectl >/dev/null 2>&1 || fail "kubectl not found" "make k8s-tools"
kc get deploy "$DEPLOY" >/dev/null 2>&1 || fail "deployment/$DEPLOY not found" "make k8s-apply"

# --- 1. spread ------------------------------------------------------------
kc rollout status "deploy/$DEPLOY" --timeout=120s >/dev/null
mapfile -t pods < <(kc get pods -l app.kubernetes.io/name="$DEPLOY" \
	-o jsonpath='{range .items[*]}{.metadata.name} {.spec.nodeName} {.status.phase}{"\n"}{end}')
running=0
declare -A nodes=()
for line in "${pods[@]}"; do
	read -r name node phase <<<"$line"
	[ "$phase" = "Running" ] || fail "pod $name is $phase" "kubectl -n $NAMESPACE describe pod $name"
	running=$((running + 1))
	nodes["$node"]=1
	echo "    $name → $node"
done
[ "$running" -eq 3 ] || fail "expected 3 Running pods, found $running"
[ "${#nodes[@]}" -ge 2 ] || fail "all 3 pods landed on one node (${!nodes[*]})" \
	"podAntiAffinity is 'preferred' — check that ≥2 agent nodes are schedulable"
echo "✔ 3 pods Running across ${#nodes[@]} distinct nodes"

# --- 2. load balancing + NODE_ID ------------------------------------------
declare -A seen=()
for _ in $(seq "$REQUESTS"); do
	node="$(kc exec "$CLIENT_POD" -- \
		curl -s -o /dev/null -D- "http://$DEPLOY:8080/healthz" 2>/dev/null |
		awk -F': ' 'tolower($1)=="x-automail-node"{gsub(/\r/,"",$2); print $2}')"
	[ -n "$node" ] || fail "no X-Automail-Node header on the response" "is the middleware still setting it?"
	seen["$node"]=$((${seen["$node"]:-0} + 1))
done
for node in "${!seen[@]}"; do
	# The header must be a POD NAME. That is the downward-API contract: it is
	# what makes `kubectl logs <value>` work straight off a response header.
	grep -qw "$node" <<<"$(pod_names)" ||
		fail "X-Automail-Node '$node' is not a current pod name" "NODE_ID is not coming from fieldRef: metadata.name"
	echo "    $node served ${seen[$node]}/$REQUESTS"
done
[ "${#seen[@]}" -ge 2 ] ||
	fail "$REQUESTS requests all hit one pod (${!seen[*]})" "the ClusterIP Service should spread across Ready endpoints"
echo "✔ Service spread $REQUESTS requests over ${#seen[@]} pods, every X-Automail-Node a real pod name"

# --- 3. consumer group before -------------------------------------------
before="$(consumers | sort)"
before_n="$(grep -c . <<<"$before" || true)"
[ "$before_n" -eq 3 ] ||
	fail "dispatchers group has $before_n consumers, expected 3:"$'\n'"$before" \
		"a count above the pod count is a leak — Goal K0's XGROUP DELCONSUMER"
echo "✔ dispatchers consumer count = 3 (matches pod count)"

# --- 4. rolling restart, watched -----------------------------------------
desired="$(kc get deploy "$DEPLOY" -o jsonpath='{.spec.replicas}')"
min_ready=$desired
kc rollout restart "deploy/$DEPLOY" >/dev/null
echo "→ rolling restart (sampling readyReplicas throughout)"
(
	kc rollout status "deploy/$DEPLOY" --timeout=180s >/dev/null
	echo done >/tmp/.k3-rollout-done.$$
) &
watcher=$!
rm -f "/tmp/.k3-rollout-done.$$"
while kill -0 "$watcher" 2>/dev/null; do
	ready="$(kc get deploy "$DEPLOY" -o jsonpath='{.status.readyReplicas}')"
	ready="${ready:-0}"
	[ "$ready" -lt "$min_ready" ] && min_ready="$ready"
	sleep 0.4
done
wait "$watcher" || fail "rollout did not complete within 180s" "kubectl -n $NAMESPACE describe deploy/$DEPLOY"
rm -f "/tmp/.k3-rollout-done.$$"
[ "$min_ready" -ge "$desired" ] ||
	fail "readyReplicas dropped to $min_ready during the rollout (desired $desired)" \
		"maxUnavailable: 0 was not honoured"
echo "✔ rolling restart complete; readyReplicas never fell below $desired (min observed: $min_ready)"

# --- 3b. consumer group after (the K0 regression guard) -------------------
# Give the drained pods' DELCONSUMER a moment to land; the old pods only
# remove themselves once their shutdown sequence reaches the consumer-group
# step, which is after the drain signal and the dispatcher loop.
for _ in $(seq 30); do
	[ "$(consumers | grep -c . || true)" -le 3 ] && break
	sleep 1
done
after="$(consumers | sort)"
after_n="$(grep -c . <<<"$after" || true)"
pods_now="$(pod_names)"
echo "    pods now:      $pods_now"
echo "    consumers now: $(tr '\n' ' ' <<<"$after")"
[ "$after_n" -eq 3 ] ||
	fail "dispatchers group has $after_n consumers after the restart, expected 3" \
		"stale consumers survived the rollout — this is exactly the Goal K0 bug"
for c in $after; do
	grep -qw "$c" <<<"$pods_now" ||
		fail "consumer '$c' is not a current pod name" "a previous generation's consumer was left behind"
done
echo "✔ after the rollout: 3 consumers, each one a current pod (no leak)"

# --- 5. mTLS survives the NodePort ---------------------------------------
# Kubernetes Track Process Rule 4: mTLS on every internal hop must survive the
# move to Services and NodePorts. TLS terminates in the pod, not in kube-proxy,
# so it should -- but "should" is not a measurement. Both directions are
# asserted: a certed client gets through, and a certless one is REFUSED (that
# refusal is the property, exactly as the Goal T3 invariant test frames it).
#
# The dial host is `localhost` deliberately: the cloud-server cert's SANs are
# `DNS:cloud-server, DNS:localhost` and services/printer/mtls.go sets no
# ServerName, so this is the one external name that verifies (§6.1 option a).
# This is the path Goal K5's printer will dial.
if [ -r infra/certs/printer-cert.pem ] && command -v curl >/dev/null 2>&1; then
	url="https://localhost:$MTLS_HOST_PORT/internal/healthz"
	body="$(curl -sf --cert infra/certs/printer-cert.pem --key infra/certs/printer-key.pem \
		--cacert infra/certs/ca-cert.pem "$url" || true)"
	grep -q '"status":"ok"' <<<"$body" ||
		fail "mTLS NodePort $MTLS_HOST_PORT did not answer with a client cert (got: ${body:-nothing})" \
			"nodePort $MTLS_NODE_PORT must match the host mapping frozen in infra/k8s/k3d-cluster.yaml"
	if curl -sf --cacert infra/certs/ca-cert.pem "$url" >/dev/null 2>&1; then
		fail "the mTLS NodePort served a client WITHOUT a certificate" \
			"tls.RequireAndVerifyClientCert is the invariant; this is a security regression"
	fi
	echo "✔ mTLS through nodePort $MTLS_NODE_PORT (host $MTLS_HOST_PORT): certed client OK, certless client refused"
else
	echo "⚠ mTLS NodePort check skipped: infra/certs/printer-cert.pem or curl missing"
fi
