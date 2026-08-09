#!/usr/bin/env bash
# Seed the minimum fixture the browser E2E needs against the running compose
# stack's Postgres:
#   - one building + one mailbox whose id matches the printer's MAILBOX_ID and
#     whose public_key_pem is the printer's real document public key (so a job
#     encrypted to it actually decrypts end-to-end);
#   - one slot + one resident ("Rivka Testmann") the guest flow searches for;
#   - one admin sender (admin@automail.test / adminpass123) -- admin is not
#     self-assignable via /auth/register, so it is seeded directly.
#
# Idempotent: fixed UUIDs + ON CONFLICT DO NOTHING. Values are non-production.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

# WHERE the `postgres` this seeds into lives. Two backends, one fixture:
#
#   compose (default)  docker compose exec, resolved against E2E_COMPOSE_FILES
#                      -- the Part 4b browser-E2E pair unless overridden by the
#                      Part 5 full-system run (scripts/e2e/full.sh).
#   kubectl            kubectl exec into postgres-0 in the Kubernetes namespace
#                      (Goal K4's browser acceptance through the ingress, and
#                      Goal K5's k8s E2E). Same SQL, same fixture UUIDs, same
#                      .env values -- the ONLY difference is how psql is
#                      reached, which is the point: one seed, two deploy targets.
SEED_BACKEND="${SEED_BACKEND:-compose}"
if [ -n "${E2E_COMPOSE_FILES:-}" ]; then
  read -ra COMPOSE_FILES <<<"$E2E_COMPOSE_FILES"
else
  COMPOSE_FILES=(-f docker-compose.yml -f infra/compose/e2e.yml)
fi
COMPOSE=(docker compose "${COMPOSE_FILES[@]}")

APP_KEY="$(grep '^APP_ENCRYPTION_KEY=' .env | cut -d= -f2-)"
PUBKEY="$(cat infra/certs/printer-public.pem)"
# bcrypt hash of "adminpass123" (cost 10). Non-production fixture credential.
ADMIN_HASH='$2a$10$CU4/Y/byNgJLpmoJje0Kq.nl7MM1f0xBhvwUCpPPTaQIKblxXDITe'

MAILBOX_ID="${DEV_MAILBOX_ID:-00000000-0000-0000-0000-000000000001}"

PGUSER_VAL="$(grep '^POSTGRES_USER=' .env | cut -d= -f2-)"
PGPASS_VAL="$(grep '^POSTGRES_PASSWORD=' .env | cut -d= -f2-)"
PGDB_VAL="$(grep '^POSTGRES_DB=' .env | cut -d= -f2-)"

case "$SEED_BACKEND" in
  compose)
    EXEC=("${COMPOSE[@]}" exec -T -e PGPASSWORD="$PGPASS_VAL" postgres)
    ;;
  kubectl)
    # `env PGPASSWORD=...` rather than a -e flag: kubectl exec has no env
    # option, so the variable is set by the command itself inside the pod.
    EXEC=(kubectl -n "${NAMESPACE:-automail}" exec -i "${SEED_POD:-postgres-0}" -- env PGPASSWORD="$PGPASS_VAL")
    ;;
  *)
    echo "✗ unknown SEED_BACKEND '$SEED_BACKEND' (expected: compose | kubectl)" >&2
    exit 1
    ;;
esac

echo "==> Seeding fixture data into Postgres (backend: $SEED_BACKEND)"
"${EXEC[@]}" psql -v ON_ERROR_STOP=1 \
  -U "$PGUSER_VAL" \
  -d "$PGDB_VAL" \
  -v app_key="$APP_KEY" -v pubkey="$PUBKEY" -v admin_hash="$ADMIN_HASH" \
  -v mailbox_id="$MAILBOX_ID" <<'SQL'
INSERT INTO buildings (id, address)
VALUES ('11111111-1111-1111-1111-111111111111', '100 Test Street, Springfield')
ON CONFLICT (id) DO NOTHING;

INSERT INTO mailboxes (id, building_id, public_key_pem, status)
VALUES (:'mailbox_id', '11111111-1111-1111-1111-111111111111', :'pubkey', 'offline')
ON CONFLICT (id) DO NOTHING;

INSERT INTO mailbox_slots (id, mailbox_id, slot_number, max_count)
VALUES ('22222222-2222-2222-2222-222222222222', :'mailbox_id', 1, 5)
ON CONFLICT (mailbox_id, slot_number) DO NOTHING;

INSERT INTO residents (id, slot_id, name_enc)
VALUES ('33333333-3333-3333-3333-333333333333',
        '22222222-2222-2222-2222-222222222222',
        pgp_sym_encrypt('Rivka Testmann', :'app_key'))
ON CONFLICT (slot_id) DO NOTHING;

INSERT INTO senders (id, email_enc, name_enc, password_hash, role)
VALUES ('44444444-4444-4444-4444-444444444444',
        pgp_sym_encrypt('admin@automail.test', :'app_key'),
        pgp_sym_encrypt('Ops Admin', :'app_key'),
        :'admin_hash', 'admin')
ON CONFLICT (id) DO NOTHING;
SQL

echo "==> Seed complete."
