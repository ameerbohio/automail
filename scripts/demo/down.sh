#!/usr/bin/env bash
# Tears the public demo down and PROVES it is gone -- the whole point of this
# script over a bare `docker compose down` is that it verifies rather than
# assumes. Leaving a tunnel up by accident is the failure mode worth guarding.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.demo.yml)

echo "==> Stopping the stack and tunnel (removing volumes)"
"${COMPOSE[@]}" down -v --remove-orphans 2>&1 | tail -3

# Belt and braces: a cloudflared started by hand outside compose would survive
# the compose teardown. `pkill -x` matches the executable name -- NEVER use
# `pkill -f 'cloudflared tunnel'`, which also matches the shell running this
# script and kills it mid-teardown.
if pgrep -x cloudflared >/dev/null 2>&1; then
  echo "==> Killing a stray host cloudflared (started outside compose)"
  pkill -x cloudflared
  sleep 2
fi

echo
echo "==> Verifying"
containers="$(docker ps -aq --filter "name=automail" | wc -l)"
volumes="$(docker volume ls -q --filter "name=automail" | wc -l)"
# `pgrep -xc` prints "0" AND exits non-zero when nothing matches, so a
# `|| echo 0` fallback appends a SECOND zero and every later string comparison
# fails against the literal "0\n0". Counting lines sidesteps the dual output.
tunnels="$(pgrep -x cloudflared 2>/dev/null | wc -l | tr -d ' ')"
printf '    automail containers : %s\n' "$containers"
printf '    automail volumes    : %s\n' "$volumes"
printf '    cloudflared procs   : %s\n' "$tunnels"

if [ "$containers" = "0" ] && [ "$volumes" = "0" ] && [ "$tunnels" = "0" ]; then
  echo "    ✔ nothing is exposed"
else
  echo "    ✗ something is still running -- inspect with \`docker ps -a\` / \`pgrep -ax cloudflared\`" >&2
  exit 1
fi
