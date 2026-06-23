#!/usr/bin/env bash
# Generates the printer microservice's RSA-4096 keypair -- the document
# key senders wrap their AES-256-GCM key with (plans/02-security.md #2).
# This is NOT the mTLS cert and NOT the JWT signing key.
#
# The private key is encrypted at rest with AES-256-CBC; the passphrase
# must be supplied via PRINTER_KEY_PASSPHRASE (never hardcoded, never
# logged). The printer microservice decrypts it into memory at startup
# and zeroes the passphrase immediately after (see plans/02-security.md).
#
# Note for Phase 6 (printer key loading): `openssl genpkey` produces a
# PKCS8 "ENCRYPTED PRIVATE KEY" (PBES2), not the legacy "DEK-Info" PEM
# format plans/02-security.md's Go snippet assumes. Go's x509.DecryptPEMBlock
# only handles the legacy format and won't parse this -- decrypt PKCS8
# directly instead (e.g. youmark/pkcs8, or openssl pkcs8 to convert first).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

if [ -z "${PRINTER_KEY_PASSPHRASE:-}" ]; then
  echo "PRINTER_KEY_PASSPHRASE must be set in the environment" >&2
  exit 1
fi

rm -f printer-private.pem printer-public.pem

echo "==> Generating printer RSA-4096 keypair (private key encrypted at rest)"
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:4096 \
  -aes-256-cbc -pass env:PRINTER_KEY_PASSPHRASE -out printer-private.pem

openssl rsa -pubout -in printer-private.pem -passin env:PRINTER_KEY_PASSPHRASE \
  -out printer-public.pem

echo "==> Done:"
ls -1 printer-private.pem printer-public.pem
