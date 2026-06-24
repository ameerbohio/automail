-- sqlc query definitions. plans/08-data-models.md is the schema source of
-- truth; these are the Phase 2 queries (recipients, jobs, audit log).
-- `app_key` arguments are pgcrypto's symmetric key for PII columns -- see
-- docs/study/07-pgcrypto-pii-encryption.md for why it travels in the SQL
-- call instead of staying purely in application code.

-- name: SearchRecipients :many
-- Resident name is pgcrypto-encrypted, so the match has to happen after
-- decrypting each row -- there is no index on plaintext name. Fine at
-- prototype scale; a real directory would need a separate searchable
-- index (e.g. a deterministic blind-index column) to avoid scanning
-- every resident row per search.
SELECT
  residents.id AS recipient_id,
  pgp_sym_decrypt(residents.name_enc, sqlc.arg(app_key)) AS full_name,
  buildings.address AS building_address
FROM residents
JOIN mailbox_slots ON mailbox_slots.id = residents.slot_id
JOIN mailboxes ON mailboxes.id = mailbox_slots.mailbox_id
JOIN buildings ON buildings.id = mailboxes.building_id
WHERE pgp_sym_decrypt(residents.name_enc, sqlc.arg(app_key)) ILIKE '%' || sqlc.arg(query)::text || '%'
   OR buildings.address ILIKE '%' || sqlc.arg(query)::text || '%'
ORDER BY full_name
LIMIT 20;

-- name: ResolveRecipient :one
-- Resolves a recipient_id (residents.id) to everything POST /jobs needs:
-- the mailbox's public key (for the sender to wrap their AES key) and the
-- slot_id/mailbox_id the sender never sees directly.
SELECT
  residents.id AS recipient_id,
  mailbox_slots.id AS slot_id,
  mailboxes.id AS mailbox_id,
  mailboxes.public_key_pem
FROM residents
JOIN mailbox_slots ON mailbox_slots.id = residents.slot_id
JOIN mailboxes ON mailboxes.id = mailbox_slots.mailbox_id
WHERE residents.id = $1;

-- name: InsertJob :one
-- Phase 2 skips dispatch logic entirely -- every job lands directly in
-- 'queued' rather than the schema default 'submitted'. Real dispatch
-- attempts (submitted -> dispatching | queued) arrive in Phase 4.
INSERT INTO jobs (
  sender_id, guest_token_hash, mailbox_id, slot_id,
  encrypted_key, blob_ref, page_count, status
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, 'queued'
)
RETURNING id, status;

-- name: InsertAuditEvent :exec
INSERT INTO audit_events (job_id, action, actor_id) VALUES ($1, $2, $3);

-- name: GetSenderByEmail :one
-- Same decrypt-then-compare approach as SearchRecipients -- email_enc has
-- no plaintext index, so this is a full scan at prototype scale.
SELECT id, email_enc, password_hash, role
FROM senders
WHERE pgp_sym_decrypt(email_enc, sqlc.arg(app_key)) = sqlc.arg(email)::text;

-- name: GetSenderByID :one
SELECT id, password_hash, role
FROM senders
WHERE id = $1;

-- name: InsertRefreshToken :exec
INSERT INTO refresh_tokens (sender_id, token_hash, expires_at)
VALUES ($1, $2, $3);

-- name: GetActiveRefreshToken :one
SELECT id, sender_id, expires_at
FROM refresh_tokens
WHERE token_hash = $1 AND revoked_at IS NULL;

-- name: RevokeRefreshTokenByHash :exec
UPDATE refresh_tokens SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL;

-- name: UpdateJobStatus :one
-- Applied from "status" printer-link frames (plans/05-cloud-server.md
-- "Status frames"). Phase 3 only ever writes 'delivered' (the dev-mode
-- stub dispatch always succeeds); 'printing' and 'failed' transitions
-- are exercised for real once Phase 4 implements actual dispatch retries.
-- delivered_at is set only on the 'delivered' transition.
UPDATE jobs
SET status = sqlc.arg(status),
    delivered_at = CASE WHEN sqlc.arg(status) = 'delivered' THEN now() ELSE delivered_at END
WHERE id = sqlc.arg(id)
RETURNING id, mailbox_id, blob_ref, status;
