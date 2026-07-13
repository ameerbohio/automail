#!/usr/bin/env bash
# Coverage gate with a *ratcheting floor*: per-module statement coverage may rise
# but must never fall below the recorded floor (scripts/coverage.floors). A floor
# that only goes up resists the gaming a fixed target invites.
#
# Covdata-free by design. `go test ./... -coverprofile` merges per-package data
# with the `covdata` tool, which the go1.25 toolchain resolved in this environment
# ships incompletely (its tool dir is missing covdata), so a merge over packages
# that have no tests fails. We therefore profile only the packages that actually
# contain _test.go files — deterministic, and it exercises the same code paths.
# CI (a complete toolchain) runs the identical script, so the number is portable.
#
# Override the floor for both modules to prove the gate fails:
#   COVER_FLOOR_OVERRIDE=99 bash scripts/coverage.sh   # expected: non-zero exit
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FLOORS="$ROOT/scripts/coverage.floors"
fail=0

tested_pkgs() { # list import paths under services/$1 that contain a _test.go file
	( cd "$ROOT/services/$1" && go list ./... | while read -r p; do
		d=$(go list -f '{{.Dir}}' "$p")
		if ls "$d"/*_test.go >/dev/null 2>&1; then echo "$p"; fi
	done )
}

floor_for() { awk -v m="$1" '$1==m{print $2}' "$FLOORS"; }

for m in cloud printer; do
	pkgs=$(tested_pkgs "$m")
	prof="$ROOT/services/$m/coverage.out"
	echo "── $m ────────────────────────────────────────────"
	# shellcheck disable=SC2086  # $pkgs is an intentional word list
	out=$( cd "$ROOT/services/$m" && go test $pkgs -covermode=atomic -coverprofile="$prof" 2>&1 )
	echo "$out" | grep -E 'coverage:|FAIL|panic' | sed 's/^/  /' || true
	total=$( cd "$ROOT/services/$m" && go tool cover -func="$prof" | awk '/^total:/{gsub("%","",$3); print $3}')
	floor=${COVER_FLOOR_OVERRIDE:-$(floor_for "$m")}
	if awk -v c="$total" -v f="$floor" 'BEGIN{ exit !(c+0 >= f+0) }'; then
		echo "  ✔ $m coverage ${total}% ≥ floor ${floor}%"
	else
		echo "  ✗ $m coverage ${total}% < floor ${floor}% — coverage must not drop"
		fail=1
	fi
done

exit $fail
