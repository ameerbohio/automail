#!/usr/bin/env bash
# Generates the internal mTLS PKI: a self-signed CA, then a cert/key pair
# for the cloud server and one for the printer microservice, both signed
# by that CA. Output lands in this directory (infra/certs/), which is
# gitignored -- never commit these.
#
# Re-running wipes and regenerates everything. For cert rotation in dev,
# just re-run this and `docker compose restart`.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

rm -f ./*.pem ./*.srl

echo "==> Generating internal CA"
openssl req -x509 -newkey rsa:4096 -keyout ca-key.pem -out ca-cert.pem \
  -days 3650 -nodes -subj "/CN=automail-internal-ca"

echo "==> Generating cloud-server certificate (signed by internal CA)"
openssl req -newkey rsa:2048 -keyout cloud-server-key.pem -out cloud-server-csr.pem \
  -nodes -subj "/CN=cloud-server"
openssl x509 -req -in cloud-server-csr.pem -CA ca-cert.pem -CAkey ca-key.pem \
  -CAcreateserial -out cloud-server-cert.pem -days 365
rm -f cloud-server-csr.pem

echo "==> Generating printer-service certificate (signed by internal CA)"
openssl req -newkey rsa:2048 -keyout printer-key.pem -out printer-csr.pem \
  -nodes -subj "/CN=printer-service"
openssl x509 -req -in printer-csr.pem -CA ca-cert.pem -CAkey ca-key.pem \
  -CAcreateserial -out printer-cert.pem -days 365
rm -f printer-csr.pem

echo "==> Verifying chain"
openssl verify -CAfile ca-cert.pem cloud-server-cert.pem
openssl verify -CAfile ca-cert.pem printer-cert.pem

echo "==> Done. Files written to $(pwd):"
ls -1 ./*.pem
