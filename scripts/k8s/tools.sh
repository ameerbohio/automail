#!/usr/bin/env bash
# Install the pinned Kubernetes tooling (Goal K1 / plans/16-kubernetes.md §2.1).
#
# k3d and kubectl were both absent on this host. They install as single static
# binaries into ~/.local/bin (already on PATH here), so this needs no sudo --
# which matters, because there is no passwordless sudo on this machine and a
# goal that silently prompts for a password is a goal that hangs.
#
# Both downloads are checksum-verified. That is not ceremony: these binaries
# get the credentials to a cluster and are fetched over the network, so
# "downloaded something and chmod +x'd it" is exactly the supply-chain step the
# rest of this repo (gitleaks, govulncheck, osv-scanner) refuses to hand-wave.
#
# Idempotent: an already-installed matching version is left alone.
#
# Env knobs:
#   BIN_DIR=~/.local/bin   where the binaries land
#   FORCE=1                reinstall even if the pinned version is present
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
source scripts/k8s/versions.env

BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
FORCE="${FORCE:-}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$BIN_DIR"

install_kubectl() {
	if [ -z "$FORCE" ] && command -v kubectl >/dev/null 2>&1 &&
		kubectl version --client 2>/dev/null | grep -qF "$KUBECTL_VERSION"; then
		echo "✔ kubectl $KUBECTL_VERSION already installed ($(command -v kubectl))"
		return
	fi
	local base="https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64"
	echo "→ downloading kubectl $KUBECTL_VERSION"
	curl -sSfL --max-time 120 -o "$TMP/kubectl" "$base/kubectl"
	curl -sSfL --max-time 60 -o "$TMP/kubectl.sha256" "$base/kubectl.sha256"
	(cd "$TMP" && echo "$(cat kubectl.sha256)  kubectl" | sha256sum -c - >/dev/null)
	install -m 0755 "$TMP/kubectl" "$BIN_DIR/kubectl"
	echo "✔ kubectl $KUBECTL_VERSION -> $BIN_DIR/kubectl (sha256 verified)"
}

install_k3d() {
	if [ -z "$FORCE" ] && command -v k3d >/dev/null 2>&1 &&
		k3d version 2>/dev/null | grep -qF "k3d version $K3D_VERSION"; then
		echo "✔ k3d $K3D_VERSION already installed ($(command -v k3d))"
		return
	fi
	local rel="https://github.com/k3d-io/k3d/releases/download/${K3D_VERSION}"
	echo "→ downloading k3d $K3D_VERSION"
	curl -sSfL --max-time 120 -o "$TMP/k3d" "$rel/k3d-linux-amd64"
	# The release ships one checksums.txt whose paths are build-relative
	# (`_dist/k3d-linux-amd64`), so match the suffix rather than the name.
	curl -sSfL --max-time 60 -o "$TMP/checksums.txt" "$rel/checksums.txt"
	local want got
	want="$(grep -E 'k3d-linux-amd64$' "$TMP/checksums.txt" | awk '{print $1}')"
	got="$(sha256sum "$TMP/k3d" | awk '{print $1}')"
	if [ -z "$want" ] || [ "$want" != "$got" ]; then
		echo "✗ k3d checksum mismatch (want=${want:-<none found>} got=$got)" >&2
		exit 1
	fi
	install -m 0755 "$TMP/k3d" "$BIN_DIR/k3d"
	echo "✔ k3d $K3D_VERSION -> $BIN_DIR/k3d (sha256 verified)"
}

install_kubectl
install_k3d

case ":$PATH:" in
*":$BIN_DIR:"*) ;;
*) echo "⚠ $BIN_DIR is not on PATH -- add it before running make k8s-up" ;;
esac

kubectl version --client | head -1
k3d version | head -1
