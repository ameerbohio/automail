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
-- Every job starts 'submitted' (the schema default). Phase 4's
-- tryDispatch runs immediately after insert and moves it to
-- 'dispatching' (immediate dispatch) or 'queued' (blocked, added to the
-- jobs:pending Redis Stream) -- see CreateJob in handlers/jobs.go.
INSERT INTO jobs (
  sender_id, guest_token_hash, mailbox_id, slot_id,
  encrypted_key, blob_ref, page_count
) VALUES (
  $1, $2, $3, $4, $5, $6, $7
)
RETURNING id, status;

-- name: InsertAuditEvent :exec
INSERT INTO audit_events (job_id, action, actor_id) VALUES ($1, $2, $3);

-- name: SetJobBlobDeleted :exec
-- Marks a delivered job's ciphertext as removed from object storage
-- (plans/05-cloud-server.md "On delivered: ... Update job blob_deleted_at").
-- Metadata only -- the zero-knowledge boundary is untouched: this never
-- reads or writes encrypted_key.
UPDATE jobs SET blob_deleted_at = now() WHERE id = sqlc.arg(id);

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

-- name: InsertSender :one
-- Phase 8 open registration (plans/09 POST /auth/register). email_enc and
-- name_enc are pgcrypto-encrypted PII like the other columns; because
-- pgp_sym_encrypt is non-deterministic there is no unique index on email_enc,
-- so the handler pre-checks GetSenderByEmail for duplicates (prototype scale --
-- a deterministic blind-index column would be the real fix). role is fixed to
-- 'sender' here and is never caller-supplied -- admin is not self-assignable.
INSERT INTO senders (email_enc, name_enc, password_hash, role)
VALUES (
  pgp_sym_encrypt(sqlc.arg(email)::text, sqlc.arg(app_key)),
  pgp_sym_encrypt(sqlc.arg(name)::text, sqlc.arg(app_key)),
  sqlc.arg(password_hash),
  'sender'
)
RETURNING id, role;

-- name: InsertRefreshToken :exec
INSERT INTO refresh_tokens (sender_id, token_hash, expires_at)
VALUES ($1, $2, $3);

-- name: GetActiveRefreshToken :one
SELECT id, sender_id, expires_at
FROM refresh_tokens
WHERE token_hash = $1 AND revoked_at IS NULL;

-- name: RevokeRefreshTokenByHash :exec
UPDATE refresh_tokens SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL;

-- name: LockJobForDispatch :one
-- Phase 4 double-dispatch guard (plans/03-scaling.md "Dispatch Eligibility
-- Check"). Must run inside a transaction alongside MarkJobDispatching --
-- NOWAIT means a second node racing for the same row gets an immediate
-- 55P03 lock_not_available error instead of blocking, so it can fall
-- through to "someone else has this" and move on to the next job rather
-- than stall a connection. Only 'submitted' or 'queued' rows are
-- claimable: 'dispatching'/'printing'/'delivered'/'failed' are already
-- spoken for or terminal.
SELECT id, mailbox_id, slot_id, encrypted_key, blob_ref, status
FROM jobs
WHERE id = sqlc.arg(id) AND status IN ('submitted', 'queued')
FOR UPDATE NOWAIT;

-- name: MarkJobDispatching :exec
-- Second half of the claim transaction started by LockJobForDispatch.
UPDATE jobs SET status = 'dispatching' WHERE id = sqlc.arg(id);

-- name: RequeueJob :exec
-- Reverts a claimed job back to 'queued' when the publish to
-- mailbox:<id>:dispatch finds zero subscribers (printer link not held by
-- any live node -- plans/05-cloud-server.md "Presence and liveness").
UPDATE jobs SET status = 'queued' WHERE id = sqlc.arg(id);

-- name: GetJobForStream :one
-- Phase 5's GET /jobs/:id/stream lookup: the two credentials a stream
-- request can be authorized against (sender_id for the JWT ownership
-- check, guest_token_hash for the ?token= check) plus the current status
-- for the initial snapshot event. Deliberately does NOT select
-- encrypted_key -- the SSE path never touches ciphertext key material
-- (plans/02-security.md "Zero-Knowledge Guarantee").
SELECT id, sender_id, guest_token_hash, status
FROM jobs
WHERE id = $1;

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

-- name: GetJobsBySender :many
-- Phase 8 /history (GET /jobs, authenticated). A sender's own jobs, newest
-- first. Metadata only -- deliberately never selects encrypted_key or blob_ref
-- (zero-knowledge; plans/02-security.md). Bounded LIMIT keeps the response
-- cheap at prototype scale.
SELECT id, status, page_count, created_at, delivered_at
FROM jobs
WHERE sender_id = $1
ORDER BY created_at DESC
LIMIT 200;

-- name: AdminListJobs :many
-- Phase 9 GET /admin/jobs (plans/09-api-contracts.md). Operator-facing job
-- table, newest first, paginated. Metadata only: like GetJobsBySender it
-- deliberately never selects encrypted_key or blob_ref -- the admin views
-- expose no ciphertext (zero-knowledge invariant, plans/02-security.md). The
-- slot_number join gives the human-readable "Slot 3" the dashboard shows
-- instead of the raw slot UUID. An empty status arg matches every status
-- (the "All" filter); a non-empty one is an exact match.
SELECT
  jobs.id,
  jobs.slot_id,
  mailbox_slots.slot_number,
  jobs.status,
  jobs.page_count,
  jobs.created_at,
  jobs.delivered_at
FROM jobs
JOIN mailbox_slots ON mailbox_slots.id = jobs.slot_id
WHERE (sqlc.arg(status)::text = '' OR jobs.status = sqlc.arg(status)::text)
ORDER BY jobs.created_at DESC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: AdminCountJobs :one
-- Total row count for the same (optionally status-filtered) set AdminListJobs
-- pages over, so GET /admin/jobs can return an accurate "total" for the UI's
-- pagination independent of the current page window.
SELECT count(*) FROM jobs
WHERE (sqlc.arg(status)::text = '' OR jobs.status = sqlc.arg(status)::text);

-- name: AdminJobStatusCounts :many
-- Phase 9 /admin overview (plans/07-ops-dashboard.md): one row per status with
-- its count. Feeds the queue-depth tally (submitted+queued+dispatching) and
-- the per-status breakdown. Metadata only.
SELECT status, count(*) AS count
FROM jobs
GROUP BY status;

-- name: AdminCountDeliveredSince :one
-- "Jobs completed today" for the overview -- delivered jobs whose delivered_at
-- is on/after the given instant (the handler passes start-of-day UTC).
SELECT count(*) FROM jobs
WHERE status = 'delivered' AND delivered_at >= sqlc.arg(since);

-- name: UpdateMailboxLiveness :exec
-- Durable mirror of the Redis printer-state cache. plans/08-data-models.md:
-- mailboxes.status + last_heartbeat_at "are the durable mirror used by the ops
-- dashboard." Live dispatch reads the 90s Redis cache (mailbox:<id>:state); the
-- printer-link hub also writes this row on every register/state frame so the
-- ops dashboard can show a real last-heartbeat time. Best-effort from the hub's
-- perspective -- a failed update is logged, never fatal to the link.
UPDATE mailboxes
SET status = sqlc.arg(status), last_heartbeat_at = now()
WHERE id = sqlc.arg(id);

-- name: AdminListMailboxes :many
-- Phase 9 GET /admin/mailboxes (plans/09-api-contracts.md). The stored mailbox
-- row + its building address. Live status and slot occupancy are NOT read from
-- here -- those come from the Redis mailbox:<id>:state cache the printer-link
-- hub keeps fresh (the DB status column is only the offline default; the hub
-- updates Redis, not this row). The handler overlays that cache per mailbox.
SELECT
  mailboxes.id AS mailbox_id,
  buildings.address AS building_address,
  mailboxes.status,
  mailboxes.last_heartbeat_at
FROM mailboxes
JOIN buildings ON buildings.id = mailboxes.building_id
ORDER BY buildings.address;

-- name: AdminListSlots :many
-- Every mailbox's configured slots (id, number, capacity), so the mailboxes
-- view can render a full "Slot 1: n/5" list even for a mailbox whose printer
-- is offline and therefore has no live Redis occupancy entry. max_count is the
-- authoritative capacity; current occupancy is overlaid from Redis.
SELECT id, mailbox_id, slot_number, max_count
FROM mailbox_slots
ORDER BY mailbox_id, slot_number;
