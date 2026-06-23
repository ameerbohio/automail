-- Automail cloud-server schema. Source of truth: plans/08-data-models.md.
-- Run automatically on first Postgres container init via
-- /docker-entrypoint-initdb.d (see docker-compose.yml) -- the "simplest
-- option" migration runner for dev (plans/10-implementation-roadmap.md
-- Phase 1). Re-running against an already-initialized volume is a no-op
-- for the official postgres image, since it only runs initdb scripts once.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE buildings (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  address    TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE mailboxes (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  building_id       UUID NOT NULL REFERENCES buildings(id),
  public_key_pem    TEXT NOT NULL,
  status            TEXT NOT NULL DEFAULT 'offline',
  last_heartbeat_at TIMESTAMPTZ,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE senders (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  email_enc     BYTEA NOT NULL,
  name_enc      BYTEA NOT NULL,
  password_hash TEXT NOT NULL,
  role          TEXT NOT NULL DEFAULT 'sender',
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE refresh_tokens (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  sender_id   UUID NOT NULL REFERENCES senders(id) ON DELETE CASCADE,
  token_hash  TEXT NOT NULL,
  expires_at  TIMESTAMPTZ NOT NULL,
  revoked_at  TIMESTAMPTZ,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ON refresh_tokens (token_hash) WHERE revoked_at IS NULL;

CREATE TABLE mailbox_slots (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  mailbox_id  UUID NOT NULL REFERENCES mailboxes(id),
  slot_number INT NOT NULL,
  max_count   INT NOT NULL DEFAULT 5,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (mailbox_id, slot_number)
);

CREATE TABLE residents (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  slot_id    UUID NOT NULL UNIQUE REFERENCES mailbox_slots(id),
  name_enc   BYTEA NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE jobs (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  sender_id        UUID REFERENCES senders(id),
  guest_token_hash TEXT,
  mailbox_id       UUID NOT NULL REFERENCES mailboxes(id),
  slot_id          UUID NOT NULL REFERENCES mailbox_slots(id),
  encrypted_key    BYTEA NOT NULL,
  blob_ref         TEXT NOT NULL,
  page_count       INT NOT NULL,
  status           TEXT NOT NULL DEFAULT 'submitted',
  retry_count      INT NOT NULL DEFAULT 0,
  blob_expires_at  TIMESTAMPTZ,
  blob_deleted_at  TIMESTAMPTZ,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  delivered_at     TIMESTAMPTZ,
  CONSTRAINT job_identity CHECK (
    sender_id IS NOT NULL OR guest_token_hash IS NOT NULL
  )
);

-- Allowed status transitions (enforced in application logic):
-- submitted -> dispatching | queued
-- queued -> dispatching
-- dispatching -> printing | queued (on dispatch failure)
-- printing -> delivered | failed
-- failed -> queued (on retry)

CREATE INDEX ON jobs (mailbox_id, status);
CREATE INDEX ON jobs (sender_id, created_at DESC);

CREATE TABLE audit_events (
  id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  job_id    UUID NOT NULL REFERENCES jobs(id),
  action    TEXT NOT NULL,
  -- Allowed actions: job_submitted | job_dispatched | job_printing |
  --                  job_delivered | job_failed | blob_deleted
  actor_id  UUID,
  timestamp TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION audit_no_mutate() RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION 'audit_events rows are immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_immutable
  BEFORE UPDATE OR DELETE ON audit_events
  FOR EACH ROW EXECUTE FUNCTION audit_no_mutate();
