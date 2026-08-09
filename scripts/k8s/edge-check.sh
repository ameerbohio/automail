#!/usr/bin/env bash
# Goal K4: what the cluster's Traefik ingress actually does, measured
# (plans/16-kubernetes.md §3, §8.1).
#
# Everything here is driven with `curl --resolve`, which is how every existing
# suite reaches the edge — /etc/hosts has no *.automail.local entries on this
# host and adding them needs root. The browser half of the acceptance lives in
# services/portal/e2e-k8s/ (see `make k8s-edge-browser`), because a CSP
# violation only exists in a browser.
#
# Checks, in order:
#   1. TLS + routing on all three hostnames.
#   2. sniStrict — an unrouted SNI must be REFUSED at the handshake. This is the
#      c8716b1 regression guard; losing it re-enables the first-deploy
#      ERR_SSL_UNRECOGNIZED_NAME_ALERT bug.
#   3. secure-headers, including the CSP carrying the edge's non-default port.
#   4. The presigned upload URL cloud-server signs — it must name the browser-
#      reachable blob origin WITH the port, or every guest upload dies with no
#      server-side error.
#   5. The guest rate limit: that it throttles, and WHAT IT KEYS ON. A passing
#      throttle alone is not proof the per-IP property survived the move behind
#      a cluster load balancer, so this measures both.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
source scripts/k8s/versions.env

PORT="$EDGE_HTTPS_PORT"
IP="${EDGE_IP:-127.0.0.1}"
PORTAL_HOST=automail.local
API_HOST=api.automail.local
BLOB_HOST=blob.automail.local

fail() {
	echo "✗ $1" >&2
	[ $# -gt 1 ] && echo "  → $2" >&2
	exit 1
}
# -k: the edge cert is self-signed by infra/certs/gen-edge-certs.sh. The
# handshake still has to SUCCEED and present a cert for the requested SNI --
# that is what check 2 pins down.
edge() { # $1=host $2=path ; extra args passed through
	local host="$1" path="$2"
	shift 2
	curl -sk --resolve "$host:$PORT:$IP" "$@" "https://$host:$PORT$path"
}

command -v curl >/dev/null 2>&1 || fail "curl not found"

# --- 1. routing ------------------------------------------------------------
code="$(edge "$PORTAL_HOST" / -o /dev/null -w '%{http_code}')"
[ "$code" = "200" ] || fail "portal origin returned $code, expected 200"
echo "✔ https://$PORTAL_HOST:$PORT/ → 200 (portal Deployment through the ingress)"

api="$(edge "$API_HOST" /healthz)"
grep -q '"status":"ok"' <<<"$api" || fail "api origin /healthz returned: $api"
node="$(edge "$API_HOST" /healthz -D- -o /dev/null | awk -F': ' 'tolower($1)=="x-automail-node"{gsub(/\r/,"",$2);print $2}')"
[[ "$node" == cloud-server-* ]] || fail "X-Automail-Node was '$node', expected a cloud-server pod name"
echo "✔ https://$API_HOST:$PORT/healthz → 200 from pod $node"

# MinIO answers an unsigned request with 403 AccessDenied — proof the S3 API is
# reachable and that the pre-signed URL is the authorization, not the route.
code="$(edge "$BLOB_HOST" / -o /dev/null -w '%{http_code}')"
[ "$code" = "403" ] || fail "blob origin returned $code, expected 403 (unsigned S3 request)"
echo "✔ https://$BLOB_HOST:$PORT/ → 403 AccessDenied (MinIO reachable, unsigned request refused)"

# --- 2. sniStrict ----------------------------------------------------------
if curl -sk --resolve "unrouted.automail.local:$PORT:$IP" \
	--max-time 10 -o /dev/null "https://unrouted.automail.local:$PORT/" 2>/dev/null; then
	fail "an unrouted SNI completed a TLS handshake" \
		"sniStrict must be true (infra/k8s/base/ingress/tlsoption.yaml) — this is the c8716b1 guard"
fi
echo "✔ sniStrict: an unrouted SNI is refused at the handshake"

# --- 3. security headers ---------------------------------------------------
headers="$(edge "$PORTAL_HOST" / -D- -o /dev/null)"
for h in strict-transport-security x-frame-options x-content-type-options referrer-policy content-security-policy; do
	grep -qi "^$h:" <<<"$headers" || fail "secure-headers did not set $h"
done
csp="$(awk -F': ' 'tolower($1)=="content-security-policy"{sub(/\r/,"");print $2}' <<<"$headers")"
# A CSP host-source without a port means the scheme's DEFAULT port. On a
# non-443 edge the connect-src MUST carry the port or the browser blocks the
# guest's ciphertext PUT with a console violation and no server-side error.
grep -q "https://$BLOB_HOST:$PORT" <<<"$csp" ||
	fail "CSP connect-src does not carry the edge port ($csp)" \
		"the k3d overlay patches secure-headers for exactly this (plans/16-kubernetes.md §8.1)"
echo "✔ secure-headers present; CSP connect-src names https://$BLOB_HOST:$PORT"

# --- 4. what the presigned upload URL points at ----------------------------
upload="$(edge "$API_HOST" /jobs/upload-url -X POST -H 'Content-Type: application/json' -d '{"content_type":"application/pdf"}')"
url="$(python3 -c 'import json,sys;print(json.load(sys.stdin).get("upload_url",""))' <<<"$upload" 2>/dev/null || true)"
[ -n "$url" ] || fail "POST /jobs/upload-url returned no upload_url: $upload"
grep -q "^https://$BLOB_HOST:$PORT/" <<<"$url" ||
	fail "presigned URL is not on the browser-reachable blob origin: ${url%%\?*}" \
		"MINIO_PUBLIC_ENDPOINT in the overlay must carry the edge port"
echo "✔ presigned upload URL is signed for https://$BLOB_HOST:$PORT (browser-reachable)"

# --- 5. the guest rate limit, and what it keys on --------------------------
# Sequential requests do NOT exhaust this: Traefik delays rather than rejects
# when the wait is short, so an effective allowance well above `burst` gets
# through if the requests are spread over even a few seconds. A parallel burst
# is what makes the limiter visible — worth knowing before writing an
# assertion that silently never fires.
N="${RATELIMIT_BURST:-200}"
codes="$(for _ in $(seq "$N"); do
	edge "$PORTAL_HOST" '/api/recipients?q=test' -o /dev/null -w '%{http_code}\n' &
done
wait)"
allowed="$(grep -c '^200$' <<<"$codes" || true)"
throttled="$(grep -c '^429$' <<<"$codes" || true)"
[ "$throttled" -gt 0 ] ||
	fail "$N parallel guest requests produced no 429" "the guest-ratelimit middleware is not on the portal guest route"
echo "✔ guest rate limit throttles: $allowed/$N allowed, $throttled/$N → 429"

# WHAT IT KEYS ON. Traefik's default sourceCriterion is the client IP, but the
# request has crossed the k3d loadbalancer container and a Service with
# externalTrafficPolicy: Cluster, either of which can SNAT. Two probes, taken
# while the bucket is still empty:
#   - a second EXTERNAL address (the host's own LAN IP): if it is also 429, all
#     external clients share one bucket — the per-IP limit is effectively global.
#   - an IN-CLUSTER client (no loadbalancer hop): if it passes, the limiter is
#     genuinely per-source and it is the LB hop that collapses the address.
host_ip="$(hostname -I | awk '{print $1}')"
second_external="$(curl -sk --resolve "$PORTAL_HOST:$PORT:$host_ip" -o /dev/null \
	-w '%{http_code}' "https://$PORTAL_HOST:$PORT/api/recipients?q=test" || echo "000")"
in_cluster="skipped"
if command -v kubectl >/dev/null 2>&1; then
	tip="$(kubectl -n kube-system get svc traefik -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)"
	if [ -n "$tip" ]; then
		in_cluster="$(kubectl -n "$NAMESPACE" exec minio-0 -- curl -sk \
			--resolve "$PORTAL_HOST:443:$tip" -o /dev/null -w '%{http_code}' \
			"https://$PORTAL_HOST/api/recipients?q=test" 2>/dev/null || echo "000")"
	fi
fi
echo "  ↳ while throttled: second external address ($host_ip) → $second_external; in-cluster client → $in_cluster"
if [ "$second_external" = "429" ]; then
	echo "  ↳ VERDICT: all external clients share one bucket. Behind the k3d loadbalancer the"
	echo "    per-IP limit behaves as a GLOBAL one — the §8.1 collapse, measured, not assumed."
	[ "$in_cluster" = "429" ] ||
		echo "    (the in-cluster client kept its own bucket, so the limiter IS per-source;"
		echo "     it is the loadbalancer hop that erases the distinction.)"
else
	echo "  ↳ VERDICT: a second external address kept its own bucket — the per-IP property survived."
fi

echo "✔ edge checks passed"
echo "  NOTE: the burst above left the guest bucket empty; it refills at ~1 request"
echo "  every 3s. Run \`make k8s-edge-browser\` before this target, or wait ~60s —"
echo "  otherwise the browser flow's own API calls are the ones that get 429'd."
