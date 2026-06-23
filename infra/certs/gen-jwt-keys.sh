#!/usr/bin/env bash
# Generates the RS256 JWT signing keypair (cloud server auth, NOT the mTLS
# PKI and NOT the printer's document-decryption key -- three unrelated RSA
# keypairs in this system, see docs/study/03-jwt-rs256-vs-hs256.md).
#
# RS256: the cloud server's private key signs access tokens; the public
# key can be handed to every cloud-server node (or any verifier) without
# ever exposing the signing secret. 2048 bits is standard for a token-
# signing key that rotates far more readily than the printer's document
# key, which is why it differs from the printer's 4096-bit key below.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

rm -f jwt-private.pem jwt-public.pem

echo "==> Generating JWT RS256 signing keypair"
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out jwt-private.pem
openssl rsa -pubout -in jwt-private.pem -out jwt-public.pem

echo "==> Done:"
ls -1 jwt-private.pem jwt-public.pem
