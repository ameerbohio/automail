# Data Models

All tables live in PostgreSQL. Generated Go code produced by `sqlc` from `schema.sql` and `queries.sql`.

---

## Tables

### `buildings`

```sql
CREATE TABLE buildings (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  address    TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Canada Post-specific fields (route code, etc.) are omitted for the prototype.

---

### `mailboxes`

```sql
CREATE TABLE mailboxes (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  building_id       UUID NOT NULL REFERENCES buildings(id),
  public_key_pem    TEXT NOT NULL,         -- RSA-4096 public key, PEM-encoded
  status            TEXT NOT NULL DEFAULT 'offline',  -- offline | idle | printing
  last_heartbeat_at TIMESTAMPTZ,           -- last liveness over the WebSocket link (register/state frame or pong)
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

`public_key_pem` is the RSA public key that senders use to wrap their AES key. It is served via the key directory endpoint.

There is no address column: the cloud server never dials the printer. The printer dials out and holds a persistent mTLS WebSocket, and is identified on that socket by its `id` (sent in the `register` frame). Liveness/availability is tracked in the Redis `mailbox:<id>:state` cache; `status` and `last_heartbeat_at` here are the durable mirror used by the ops dashboard.

---

### `senders`

```sql
CREATE TABLE senders (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  email_enc     BYTEA NOT NULL,   -- pgp_sym_encrypt(email, app_key)
  name_enc      BYTEA NOT NULL,   -- pgp_sym_encrypt(name, app_key)
  password_hash TEXT NOT NULL,    -- bcrypt
  role          TEXT NOT NULL DEFAULT 'sender',  -- sender | admin
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Email and name are encrypted at the application layer using pgcrypto before insertion. The `app_key` is injected at runtime via environment variable.

---

### `refresh_tokens`

```sql
CREATE TABLE refresh_tokens (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  sender_id   UUID NOT NULL REFERENCES senders(id) ON DELETE CASCADE,
  token_hash  TEXT NOT NULL,    -- SHA-256 hash of the raw token
  expires_at  TIMESTAMPTZ NOT NULL,
  revoked_at  TIMESTAMPTZ,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ON refresh_tokens (token_hash) WHERE revoked_at IS NULL;
```

---

### `mailbox_slots`

```sql
CREATE TABLE mailbox_slots (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  mailbox_id  UUID NOT NULL REFERENCES mailboxes(id),
  slot_number INT NOT NULL,
  max_count   INT NOT NULL DEFAULT 5,  -- max pieces before slot is "full"
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (mailbox_id, slot_number)
);
```

`max_count` is the capacity threshold — when `current_count` in the printer's heartbeat equals `max_count` for a slot, no new jobs are dispatched to it.

Note: `current_count` is tracked in the printer microservice's in-memory state and reported via heartbeat. It is not a column in this table — the heartbeat cache in Redis is the authoritative source.

---

### `residents`

```sql
CREATE TABLE residents (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  slot_id    UUID NOT NULL UNIQUE REFERENCES mailbox_slots(id),
  name_enc   BYTEA NOT NULL,   -- pgp_sym_encrypt(full name, app_key)
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

One resident per slot (`UNIQUE` on `slot_id`). A slot with no matching row in this table is unassigned. The sender-facing API resolves a `recipient_id` (which is this `id`) to a `slot_id` and then to a `mailbox_id` — the sender never observes the slot or mailbox identifiers directly.

---

### `jobs`

```sql
CREATE TABLE jobs (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  sender_id        UUID REFERENCES senders(id),     -- NULL for guest submissions
  guest_token_hash TEXT,                             -- SHA-256 of one-time token; only set when sender_id IS NULL
  mailbox_id       UUID NOT NULL REFERENCES mailboxes(id),
  slot_id          UUID NOT NULL REFERENCES mailbox_slots(id),
  encrypted_key    BYTEA NOT NULL,   -- RSA-OAEP ciphertext of the AES-256-GCM key
  blob_ref         TEXT NOT NULL,    -- MinIO object key (e.g. "blobs/<uuid>")
  page_count       INT NOT NULL,
  status           TEXT NOT NULL DEFAULT 'submitted',
  retry_count      INT NOT NULL DEFAULT 0,
  blob_expires_at  TIMESTAMPTZ,      -- nullable: reprint retention window (policy TBD)
  blob_deleted_at  TIMESTAMPTZ,      -- set when MinIO object is confirmed deleted
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  delivered_at     TIMESTAMPTZ,
  CONSTRAINT job_identity CHECK (
    sender_id IS NOT NULL OR guest_token_hash IS NOT NULL
  )
);

-- Allowed status transitions (enforced in application logic):
-- submitted → dispatching | queued
-- queued → dispatching
-- dispatching → printing | queued (on dispatch failure)
-- printing → delivered | failed
-- failed → queued (on retry)

CREATE INDEX ON jobs (mailbox_id, status);
CREATE INDEX ON jobs (sender_id, created_at DESC);
```

`encrypted_key` is stored as raw bytes. The cloud server never interprets it — it is forwarded as-is to the printer microservice.

**Reprint policy note**: `blob_expires_at` and `blob_deleted_at` support a future reprint window. For the prototype, `blob_deleted_at` is set immediately on delivery. `blob_expires_at` remains null until the retention policy is decided.

---

### `audit_events`

```sql
CREATE TABLE audit_events (
  id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  job_id    UUID NOT NULL REFERENCES jobs(id),
  action    TEXT NOT NULL,
  -- Allowed actions: job_submitted | job_dispatched | job_printing |
  --                  job_delivered | job_failed | blob_deleted
  actor_id  UUID,               -- sender_id or null for system actions
  timestamp TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

No document content is stored here. The audit log records lifecycle events only.

**Immutability trigger** (no UPDATE or DELETE allowed):

```sql
CREATE OR REPLACE FUNCTION audit_no_mutate() RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION 'audit_events rows are immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_immutable
  BEFORE UPDATE OR DELETE ON audit_events
  FOR EACH ROW EXECUTE FUNCTION audit_no_mutate();
```

---

## Field Sensitivity Classification

| Field | Sensitivity | Storage |
|---|---|---|
| `senders.email_enc` | High (PII) | pgcrypto encrypted |
| `senders.name_enc` | High (PII) | pgcrypto encrypted |
| `residents.name_enc` | High (PII) | pgcrypto encrypted |
| `senders.password_hash` | High | bcrypt hash (not reversible) |
| `jobs.guest_token_hash` | Medium (access credential for guest job tracking) | SHA-256 hash only — raw token never stored |
| `jobs.encrypted_key` | High (cryptographic key material) | Raw ciphertext — opaque to DB |
| `jobs.blob_ref` | Medium (locator for encrypted blob) | Plaintext path |
| `jobs.status` | Low | Plaintext |
| `audit_events.*` | Low (no content) | Plaintext |
| `mailboxes.public_key_pem` | Low (public key — safe to share) | Plaintext |

---

## sqlc Configuration

```yaml
# sqlc.yaml
version: "2"
sql:
  - engine: postgresql
    queries: services/cloud/db/queries.sql
    schema: services/cloud/db/schema.sql
    gen:
      go:
        package: db
        out: services/cloud/db
        emit_json_tags: true
        emit_prepared_queries: false
```
