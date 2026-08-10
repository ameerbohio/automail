#!/usr/bin/env bash
# Bring the printer up OUTSIDE the cluster and LEAVE IT RUNNING, so jobs
# submitted from a browser are printed rather than simulated. `make
# k8s-printer-up`.
#
# This is the gap between `make k8s-e2e` and an actually usable deployment:
# e2e.sh starts a printer, drives ONE scripted job through it and tears it back
# down, which proves the path works but leaves nothing behind to print with.
#
#   PRINT=host make k8s-printer-up    # real paper via the host's cupsd
#   make k8s-printer-up               # DEV_MODE: jobs reach "delivered", no paper
#   make k8s-printer-down             # stop it
#
# The CLUSTER needs no changes for either mode. The printer is a mailbox-unit
# process that dials IN over mTLS (plans/16-kubernetes.md §6); nothing about
# printing is scheduled by Kubernetes, so DEV_MODE is a property of this
# container alone.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

# BEFORE the source: lib-printer.sh reads PRINT at source time to build
# PRINTER_COMPOSE, and teardown must be built from the same file list as
# bring-up. Defaulting to `host` here (rather than inheriting the unset =
# DEV_MODE default the test suites want) would be a surprise, so it is not
# done — printing is opt-in in exactly one place, the command line.
export PRINT="${PRINT:-}"
# shellcheck source=scripts/k8s/lib-printer.sh
source scripts/k8s/lib-printer.sh

preflight_cluster
resolve_minio_host
start_printer
wait_printer_idle
owner="$(find_socket_owner)"

if [ "$PRINT_MODE" = "host" ]; then
	mode="PRINTING for real on host CUPS queue '$PRINTER_NAME'"
else
	mode="DEV_MODE — jobs reach \"delivered\", no paper. Re-run with PRINT=host for paper."
fi

cat <<EOF

  ┌─────────────────────────────────────────────────────────────────────┐
     PRINTER UP (outside the cluster, dialed into it)

     Mode        ${mode}
     Socket held by  ${owner}
     Submit at   https://automail.local:${EDGE_HTTPS_PORT}

     Follow it:  docker logs -f ${PRINTER_CONTAINER}
     Stop it:    make k8s-printer-down
  └─────────────────────────────────────────────────────────────────────┘

  NOTE — the slot fills after 5 jobs. Slot occupancy lives in the printer
  PROCESS (services/printer/print.go, newSlotState: Current 0, Max 5) and only
  ever increments; it resets when the process restarts, not when a job is
  collected. Job 6 will sit in \`queued\` with nothing logged as an error,
  because the cloud's eligibility check correctly sees a full mailbox. That is
  a fixture limit, not a bug — \`make k8s-printer-down && make k8s-printer-up\`
  clears it.

EOF
