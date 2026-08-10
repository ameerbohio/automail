#!/usr/bin/env bash
# Stop the standalone printer started by scripts/k8s/printer-up.sh.
# `make k8s-printer-down`.
#
# PRINT is irrelevant to teardown — printer_down exports the K8S_MINIO_HOST_IP
# placeholder for the same reason it does everywhere else (Compose interpolates
# on `down` too, and a cleanup that cannot run is worse than an unvalidated
# one), and `down` removes the project's containers regardless of which -f list
# built them.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

export PRINT="${PRINT:-}"
# shellcheck source=scripts/k8s/lib-printer.sh
source scripts/k8s/lib-printer.sh

printer_down
echo "✔ printer stopped ($PRINTER_CONTAINER)"
