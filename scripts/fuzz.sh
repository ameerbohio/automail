#!/usr/bin/env bash
# Run every Go fuzz target briefly (regression sweep, not a soak). Discovers
# `func Fuzz...` across both modules. Fuzz targets are populated in Goal T4
# (Part 1) — until then this prints a notice and exits clean.
#   FUZZTIME=1m bash scripts/fuzz.sh   # longer local run
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FUZZTIME="${FUZZTIME:-20s}"
found=0

for m in cloud printer; do
	cd "$ROOT/services/$m"
	while read -r file; do
		[ -z "$file" ] && continue
		pkg="./$(dirname "$file")"
		for fn in $(grep -hoE '^func (Fuzz[A-Za-z0-9_]+)' "$file" | awk '{print $2}'); do
			found=1
			echo "── fuzz $m $pkg $fn (${FUZZTIME}) ──"
			go test "$pkg" -run '^$' -fuzz "^$fn$" -fuzztime="$FUZZTIME"
		done
	done < <(grep -rlE '^func Fuzz' --include='*_test.go' . 2>/dev/null || true)
done

[ "$found" -eq 0 ] && echo "no fuzz targets yet (added in Goal T4)"
exit 0
