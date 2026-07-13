#!/usr/bin/env bash
# Portal (Vitest/v8) coverage gate with a ratcheting floor, mirroring the Go
# coverage.sh (Goal T6 / Part 4a). Coverage is measured over the portal's logic
# layer (lib/**, see vitest.config.ts); the React UI is covered by the Playwright
# E2E in Goal T7. The floor may rise, never fall.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
floor=${PORTAL_COVER_FLOOR_OVERRIDE:-$(awk '$1=="portal"{print $2}' "$ROOT/scripts/coverage.floors")}

cd "$ROOT/services/portal"
npx --no-install vitest run --coverage >/dev/null 2>&1
total=$(node -e "console.log(require('./coverage/coverage-summary.json').total.statements.pct)")

if awk -v c="$total" -v f="$floor" 'BEGIN{ exit !(c+0 >= f+0) }'; then
	echo "  ✔ portal coverage ${total}% ≥ floor ${floor}%"
else
	echo "  ✗ portal coverage ${total}% < floor ${floor}% — coverage must not drop"
	exit 1
fi
