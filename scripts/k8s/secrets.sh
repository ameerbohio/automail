#!/usr/bin/env bash
# Create the cluster's Secrets from the files that already hold them
# (Goal K2 / plans/16-kubernetes.md §5).
#
# WHY IMPERATIVE, NOT A MANIFEST: a Kubernetes Secret is base64, not
# encryption. Committing one is committing the credential. The whole point of
# this track's Process Rule 4 is that `make scan` (gitleaks) stays the
# authoritative gate and never has to be widened for infra/k8s/, so the key
# material stays where it already lives -- infra/certs/, infra/traefik/ and
# .env, all gitignored -- and only ever enters the cluster through this script.
# The manifests reference these Secrets by NAME. No value is ever rendered by
# `kubectl kustomize`.
#
# TWO TRUST DOMAINS, TWO SECRETS (§5, and commit c8716b1 which separated them):
#
#   automail-certs        internal mTLS PKI + JWT keypair. Private to the
#                         cluster; the CA here signs cloud-server and printer
#                         certs and nothing else.
#   automail-edge-tls     the browser-facing edge cert (kubernetes.io/tls),
#                         used by K4's IngressRoute. A completely different
#                         trust domain -- merging the two would undo the
#                         separation c8716b1 deliberately created.
#   automail-credentials  DB / object-store / app credentials, read from .env.
#
# Idempotent: re-running updates the Secrets in place (create --dry-run |
# apply), so a rotated cert is one `make k8s-secrets` away.
#
# Every input is validated BEFORE anything is created: a half-populated Secret
# fails later, in a pod, as an unexplained CrashLoop.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
source scripts/k8s/versions.env

ENV_FILE="${ENV_FILE:-.env}"
CERT_DIR=infra/certs
EDGE_DIR=infra/traefik

# Keys the cluster needs out of .env. POSTGRES_*/MINIO_* feed the data tier
# (K2); APP_ENCRYPTION_KEY feeds cloud-server's pgcrypto PII columns (K3).
# REDIS_PASSWORD is deliberately absent -- it is not wired up anywhere (see the
# comment in redis-statefulset.yaml); putting it in the Secret would imply a
# protection that does not exist.
ENV_KEYS=(
	POSTGRES_USER POSTGRES_PASSWORD POSTGRES_DB
	MINIO_ROOT_USER MINIO_ROOT_PASSWORD MINIO_KMS_SECRET_KEY
	APP_ENCRYPTION_KEY
)

# Internal PKI + JWT keypair, mounted into cloud-server by K3. Names on the
# left are the Secret keys, which become the filenames when mounted -- keep
# them equal to the on-disk names so MTLS_*_PATH values read the same in both
# deployment targets.
CERT_FILES=(
	ca-cert.pem
	cloud-server-cert.pem
	cloud-server-key.pem
	jwt-private.pem
	jwt-public.pem
)

fail() {
	echo "✗ $1" >&2
	[ $# -gt 1 ] && echo "  → $2" >&2
	exit 1
}

command -v kubectl >/dev/null 2>&1 || fail "kubectl not found" "make k8s-tools"
kubectl cluster-info >/dev/null 2>&1 ||
	fail "no reachable cluster (context: $(kubectl config current-context 2>/dev/null || echo none))" "make k8s-up"

# --- validate every input first -------------------------------------------
[ -f "$ENV_FILE" ] || fail "$ENV_FILE not found" "cp .env.example .env, then fill in every changeme* value"

missing=()
for f in "${CERT_FILES[@]}"; do [ -s "$CERT_DIR/$f" ] || missing+=("$CERT_DIR/$f"); done
if [ "${#missing[@]}" -gt 0 ]; then
	printf '✗ missing internal PKI / JWT material:\n' >&2
	printf '    %s\n' "${missing[@]}" >&2
	fail "cannot build the $CERT_SECRET Secret" "run ./infra/certs/gen.sh and ./infra/certs/gen-jwt-keys.sh"
fi
for f in edge-cert.pem edge-key.pem; do
	[ -s "$EDGE_DIR/$f" ] || fail "missing $EDGE_DIR/$f (browser-facing edge cert)" "run ./infra/certs/gen-edge-certs.sh"
done

# Read .env without sourcing it: `source` would execute whatever is in there
# and would also import every unrelated variable into this shell.
envval() { sed -n "s/^$1=//p" "$ENV_FILE" | tail -1; }

missing=()
for k in "${ENV_KEYS[@]}"; do
	v="$(envval "$k")"
	[ -n "$v" ] || missing+=("$k")
	case "$v" in changeme*) echo "⚠ $k is still a .env.example placeholder ($v)" >&2 ;; esac
done
if [ "${#missing[@]}" -gt 0 ]; then
	printf '✗ %s is missing or empty for: %s\n' "$ENV_FILE" "${missing[*]}" >&2
	fail "cannot build the $CRED_SECRET Secret" "fill these in against .env.example"
fi

# --- namespace ------------------------------------------------------------
# Created here rather than as a manifest: a Secret cannot be applied into a
# namespace that does not exist, so bootstrap has to own it. The base
# kustomization says the same thing from the other side.
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
echo "✔ namespace $NAMESPACE"

# --- credentials ----------------------------------------------------------
# Written to a 0600 temp file and passed with --from-env-file rather than
# --from-literal: literals land in this process's argv, which is world-readable
# through /proc on a shared host. Same reason the certs go in by path.
tmp_env="$(mktemp)"
chmod 600 "$tmp_env"
trap 'rm -f "$tmp_env"' EXIT
for k in "${ENV_KEYS[@]}"; do printf '%s=%s\n' "$k" "$(envval "$k")" >>"$tmp_env"; done

kubectl -n "$NAMESPACE" create secret generic "$CRED_SECRET" \
	--from-env-file="$tmp_env" --dry-run=client -o yaml |
	kubectl apply -f - >/dev/null
echo "✔ secret/$CRED_SECRET (${#ENV_KEYS[@]} keys from $ENV_FILE)"

# --- internal mTLS PKI + JWT keypair --------------------------------------
cert_args=()
for f in "${CERT_FILES[@]}"; do cert_args+=(--from-file="$f=$CERT_DIR/$f"); done
kubectl -n "$NAMESPACE" create secret generic "$CERT_SECRET" \
	"${cert_args[@]}" --dry-run=client -o yaml |
	kubectl apply -f - >/dev/null
echo "✔ secret/$CERT_SECRET (${#CERT_FILES[@]} files from $CERT_DIR — internal trust domain)"

# --- browser-facing edge cert (separate trust domain) ---------------------
kubectl -n "$NAMESPACE" create secret tls "$EDGE_SECRET" \
	--cert="$EDGE_DIR/edge-cert.pem" --key="$EDGE_DIR/edge-key.pem" \
	--dry-run=client -o yaml |
	kubectl apply -f - >/dev/null
echo "✔ secret/$EDGE_SECRET (kubernetes.io/tls from $EDGE_DIR — edge trust domain, used by K4)"

echo
kubectl -n "$NAMESPACE" get secrets "$CRED_SECRET" "$CERT_SECRET" "$EDGE_SECRET"
echo "✔ secrets in place. Nothing above touched the repo — the key material never left"
echo "  infra/certs/, infra/traefik/ and $ENV_FILE, all of which are gitignored."
