#!/usr/bin/env bash
# Generates the BROWSER-FACING edge TLS certificate Traefik serves for the public
# hostnames (automail.local, api.automail.local). This is a self-signed *server*
# certificate -- browsers show a "not secure" warning, which is expected and fine
# for this self-hosted deploy target. It is what stops the first-deploy
# ERR_SSL_UNRECOGNIZED_NAME_ALERT: with sniStrict enabled and no cert whose SAN
# matches the routed hostnames, Traefik hard-rejects every TLS handshake.
#
# IMPORTANT -- this is NOT the internal mTLS CA (gen.sh). Two separate trust
# domains, deliberately not cross-wired:
#   - gen.sh            -> internal mTLS PKI, secures the cloud <-> printer hop
#   - gen-edge-certs.sh -> edge server cert, secures the browser <-> Traefik hop
#
# Output goes to infra/traefik/ (Traefik's dynamic-config dir, already mounted
# into the container), NOT infra/certs/. That keeps the edge cert physically
# apart from the internal PKI, out of reach of gen.sh's `rm *.pem`, and limits
# what the Traefik container can read to just its own cert (it never sees the CA
# key, the app JWT keys, or the printer's document key). Both files are
# gitignored (*.pem).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../traefik"

echo "==> Generating self-signed edge TLS cert (SAN: automail.local, api.automail.local)"
openssl req -x509 -newkey rsa:2048 -keyout edge-key.pem -out edge-cert.pem \
  -days 365 -nodes -subj "/CN=automail.local" \
  -addext "subjectAltName=DNS:automail.local,DNS:api.automail.local"

echo "==> Verifying SANs"
openssl x509 -in edge-cert.pem -noout -ext subjectAltName

echo "==> Done. Files written to $(pwd):"
ls -1 edge-cert.pem edge-key.pem
