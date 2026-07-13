# Runbook

Diagnosis for the operational failures most likely to page someone. Each scenario
maps to a resilience scenario that Goal T9 (Part 7, `make chaos`) automates once
Docker is available — until then these are the manual steps.

**Observability you can rely on.** Cloud logs are structured with a subsystem
prefix and a **correlation ID**: `dispatch: job <job_id>: …`, `printer-link:
mailbox <mailbox_id>: …`, `stream: job <job_id>: …`. The printer logs `printer:
… mailbox <mailbox_id>`. Grep by `job_id` or `mailbox_id` to follow one job or one
mailbox end to end. No secret (`encrypted_key`, plaintext, passphrase) is ever
logged — enforced by the Part 6 guards.

Redis keys: `jobs:pending` (Streams queue), `mailbox:<id>:state` (printer state
cache, 90s TTL), `mailbox:<id>:dispatch` / `:available` (pub/sub), `job:<id>:status`
(SSE fan-out).

---

## A job is stuck (never reaches `delivered`)

**Triage the status first** — Postgres is the source of truth:
`SELECT id, status, mailbox_id, slot_id FROM jobs WHERE id = '<job_id>';`

- **`queued`/`submitted` and not moving** — dispatch never fired. Check the printer
  is online: `redis-cli EXISTS mailbox:<id>:state` (absent ⇒ offline, TTL lapsed —
  see next scenario). If the printer *is* online, look for the job in the pending
  stream: `redis-cli XLEN jobs:pending` and `XRANGE jobs:pending - +`. A job sitting
  in the stream with an idle printer points at the dispatcher goroutine — grep
  `dispatch: job <job_id>:`. A job in a consumer's PEL after a node crash is
  reclaimed by the periodic `XAUTOCLAIM` sweep; confirm it eventually re-dispatches.
- **`printing` and stuck** — the printer accepted it but never reported terminal
  status. Grep the printer logs for the job; check the CUPS queue (`lpstat -o`) on
  the mailbox host. The tmpfs file (`/dev/shm/automail-<job_id>.pdf`) should be
  **absent** if delivery completed; its presence means the printer died mid-print.
- **`failed`** — expected terminal state; the generic error is intentional (no
  decrypt-oracle). The specific cause is in the printer's local log only.

---

## A printer (mailbox) shows offline / disconnected

The state cache has a 90s TTL; "offline" means no `register`/`state`/keepalive
refreshed it. Order of checks:

1. Printer process alive? Its `/healthz` liveness endpoint returns 200 even mid
   reconnect (`"connected": false`) — a 200 with `connected:false` means it's
   dialing, not dead.
2. mTLS reachability — the printer dials `wss://cloud-server:8443/internal/
   printer-link`. A certless/expired client cert is **refused at the handshake**
   (by design); grep the printer log for backoff/reconnect lines.
3. On reconnect, the printer re-`register`s and the cache re-seeds. In-flight jobs
   should re-queue, not vanish (Part 7 backpressure scenario). If a job was lost,
   that's a bug, not expected.

---

## The pending stream is backing up (`jobs:pending` growing)

`redis-cli XLEN jobs:pending` climbing means jobs arrive faster than they drain —
usually a printer offline or slot-full while submissions continue.

1. Confirm the target mailbox(es) are online and `idle` (`redis-cli HGETALL
   mailbox:<id>:state`). A full slot or offline printer blocks drain — that's
   correct backpressure, not loss.
2. When the printer returns and publishes `mailbox:<id>:available`, the dispatcher
   re-evaluates the stream and drains it **exactly once** (no dup, no loss — the
   Part 7 invariant).
3. Check the consumer group for a stalled/crashed consumer:
   `redis-cli XINFO GROUPS jobs:pending` — a large PEL with no active consumer is
   reclaimed by the `XAUTOCLAIM` sweep. If lag is genuine load (not a stuck
   consumer), that's the Part 8 (k6) capacity question, not an incident.

---

## Escalation boundary

Physical-print failures (paper jam, printer offline at the hardware level) are the
owner-blocked Phase 10 boundary — outside what the software can self-heal.
