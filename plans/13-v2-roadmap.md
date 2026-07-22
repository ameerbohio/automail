# V2 Roadmap — Deferred Work & Future Ideas

> **Scope banner.** This file is **not** part of the V1 specification. Everything
> in `plans/00`–`plans/12` describes the system that is being built now (V1: the
> end-to-end encrypted demo with the real dispatch/crypto/security model). This
> file is the **parking lot for V2** — ideas the author intends to build *later*
> and deliberately does **not** want in V1. Nothing here is goal-tracked in
> `GOALS.md`, and nothing here should be implemented as part of a V1 phase or
> testing goal. If an item graduates into active work, promote it into a proper
> `plans/` section (+ a roadmap phase) and remove it from here.

## How to use this doc

Add each idea as its own `##` section with this shape, so a cold reader (or a
future agent) can pick it up without re-deriving the context:

- **What** — the change in one or two sentences.
- **Why V2, not V1** — what makes it out of scope for the current build.
- **Current V1 state** — what exists today that this builds on or replaces.
- **Sketch / considerations** — enough of a design hint to resume from, and the
  hard parts to watch.
- **References** — the `plans/` sections and code that are relevant.

---

## Separated field-unit deployment (printer node against a remote cloud)

- **What.** Deploy a printer microservice as an independent field unit on its own
  hardware (Raspberry-Pi-class box at a mailbox bank), dialing a **remote,
  already-running** cloud over the public internet — rather than as a co-located
  Docker Compose service on the same host as the cloud. Includes per-unit
  identity and mTLS certificate **provisioning** (each unit gets its own client
  cert, `MAILBOX_ID`/`SLOT_ID`, and its document keypair), and the operational
  path to enrol / revoke a unit.

- **Why V2, not V1.** V1's goal is to demonstrate the whole system end-to-end on
  one host; the printer sits in the compose stack purely as a dev convenience.
  The dispatch *mechanism* is already the production one — only the network path
  differs. Splitting the printer onto separate hardware adds fleet provisioning,
  public-internet TLS/DNS, and per-unit secret management that V1 doesn't need to
  prove the concept.

- **Current V1 state.** The printer binary already has **no co-location
  assumption**: identity, the cloud endpoint (`CLOUD_SERVER_WS_URL`), and its
  mTLS cert are all env/file config, so the same image that runs in compose is
  what a field unit runs — only the values differ. The dial-out WebSocket,
  per-node connection registry, and Redis cross-node dispatch routing are
  implemented and proven (roadmap Phases 3–4; the two-node fan-in/fan-out is
  exercised by Testing Goal T8 / `make test-e2e-full`).

- **Sketch / considerations.**
  - Point `CLOUD_SERVER_WS_URL` at the public cloud (`wss://cloud.<domain>/internal/printer-link`); the printer accepts no inbound connections, so only its one outbound socket + local healthcheck exist.
  - Per-unit provisioning is the real new work: issuing/rotating client certs against the internal CA, seeding the unit's `mailboxes`/`mailbox_slots` rows and its document public key, and a revocation path for a lost/tampered unit.
  - Not covered by Testing Goal **T12** (deploy parity), which only brings up the *whole stack* on one Proxmox host — it does not cover the separated field host or the public network path.

- **References.** `plans/01-architecture.md` "Production Considerations:
  Field-Deployed Printers"; `plans/04-printer-microservice.md` "Dispatch Model:
  Printer Dials Out" + Configuration; `plans/11-ai-collaboration.md` (NAT problem
  `[accepted]`, dispatch inversion `[implemented in demo]`).

---

## Swappable dispatch transport: `DISPATCH_MODE = push | poll`

- **What.** Make the printer↔cloud dispatch transport a swappable layer over a
  shared pipeline: keep the persistent-push WebSocket for the demo, but add a
  **jittered-polling** mode as the production model for fleet scale.

- **Why V2, not V1.** At the real target (~12M shared-mailbox units, ~30M
  people), holding a persistent socket per unit doesn't scale: the server-side
  cost of maintaining millions of open sockets and the per-device radio/CPU power
  of an always-on connection make **jittered polling** the right production model
  (with TLS session resumption to avoid repeat asymmetric handshakes). V1 keeps
  persistent push because it gives low latency and showcases the cross-node
  fan-in — the interview point. This is a decided design intent that was **never
  specced into `plans/03` nor implemented**.

- **Current V1 state.** Only persistent push exists (`link.Hub` + the printer's
  dial-out read loop). There is no `DISPATCH_MODE` config, no polling path, and
  no shared abstraction between the two.

- **Sketch / considerations.**
  - Factor the dispatch pipeline (eligibility → claim via `FOR UPDATE NOWAIT` → deliver) so the transport is pluggable; push writes to the held socket, poll answers a printer's authenticated "any jobs for `<mailbox_id>`?" request.
  - Power efficiency is the primary operating-cost lever — polling cadence + jitter is the tuning knob; latency tolerance is the trade.
  - TLS session resumption matters at this scale to avoid a full handshake per poll.

- **References.** `plans/03-scaling.md` (dispatch model); `plans/11-ai-collaboration.md`
  (the push-vs-poll decision at 12M-unit scale).

---

## Bulk mail uploads (one sender, many recipients, one submission)

- **What.** Let a sender submit a batch — a letting agency posting 400 rent
  notices, a council sending a ward-wide letter — as a single operation:
  upload a set of PDFs plus a manifest mapping each document to a recipient,
  and track the batch as one unit.

- **Why V2, not V1.** V1's job model is deliberately one document → one
  recipient → one slot, because that is what makes the crypto and dispatch
  story provable end-to-end. Batching adds a whole second entity (`batches`),
  partial-failure semantics, admission control against slot capacity, and a
  browser-side performance problem that does not exist at N=1. None of it
  changes the security model, so none of it earns its place in V1.

- **Current V1 state.** `POST /jobs` takes exactly one `{encrypted_key,
  blob_ref, recipient_id, page_count}`; `POST /jobs/upload-url` presigns
  exactly one PUT. The portal encrypts one `ArrayBuffer` on the main thread
  and streams one job's status over SSE. Nothing in the schema groups jobs.

- **Sketch / considerations.**
  - **Encryption is per-recipient and cannot be shared.** Each document is
    wrapped for a different mailbox's RSA public key, so a batch of N is N
    independent hybrid encryptions — there is no "encrypt once, fan out". This
    is the correct property (a shared key would break the whole model), and it
    is also the cost driver.
  - **Move encryption off the main thread.** N × (RSA-OAEP wrap + AES-GCM over
    up to 20 MB) will jank the tab. A Web Worker pool, and streaming the
    ciphertext into the presigned PUT rather than buffering N × 20 MB in RAM.
    This is the point where the V1 "just call `crypto.subtle`" answer stops
    scaling — a good interview beat.
  - **Batch entity.** `batches` table + `jobs.batch_id`; batch status is an
    aggregate (`queued/partial/complete/failed`), never a lock — one bad
    recipient must not fail 399 good ones. Partial failure is the default
    expectation, not the exception.
  - **Admission control.** A slot holds a bounded number of documents. Today
    dispatch checks capacity per job; a 400-job batch aimed at 30 mailboxes can
    overrun slots and strand jobs in `queued`. Check aggregate capacity at
    batch-accept time and reject/split up front, with a clear error, rather
    than discovering it one dispatch at a time.
  - **Presign burst.** N presign round-trips is the obvious bottleneck — add a
    batch presign endpoint returning N URLs in one call, and keep the existing
    single endpoint for the normal flow.
  - **Resumability.** A 400-file upload that dies at 380 must resume, not
    restart. Client-side manifest state + idempotent job creation keyed on
    `(batch_id, manifest_row)`.
  - **Status.** One SSE stream per job does not scale to a batch view. Either a
    batch-level stream that emits aggregate transitions, or the portal polls a
    batch summary and only opens per-job streams on expand.
  - **Abuse.** Bulk send is the natural spam/DoS vector — per-sender quotas and
    rate limits become mandatory, where V1 can get away without them.

- **References.** `plans/06-sender-portal.md` (encryption + submission flow);
  `plans/08-data-models.md` (`jobs`, slot capacity); `plans/09-api-contracts.md`
  (`POST /jobs`, `POST /jobs/upload-url`); `docs/study/16-hybrid-encryption.md`,
  `docs/study/18-web-crypto-e2ee-portal.md`.

---

## Recipient notifications ("you have mail in your box")

- **What.** Tell the recipient that something has been printed into their
  mailbox — a push/email/SMS notification when a job reaches `delivered`,
  and/or a physical indicator on the mailbox unit itself.

- **Why V2, not V1.** The recipient is a **passive entity** in V1: they exist as
  a row with a mailbox slot and a document public key, and they have no account,
  no session, and no contact channel. Adding one means recipient identity,
  consent capture, contact-detail PII, notification preferences, and an
  unsubscribe path — a product surface, not a systems-design one, and none of it
  proves anything about the encryption or dispatch model.

- **Current V1 state.** `delivered` is reported by the printer over the link and
  fans out to the *sender* over SSE. `recipients` holds PII in pgcrypto-encrypted
  columns and search results are masked (`Rivka Testmann` → `R. Testmann`), so
  the encrypted-PII pattern a contact channel would reuse already exists. There
  is no recipient-facing endpoint of any kind.

- **Sketch / considerations.**
  - **The zero-infrastructure option first: a physical indicator.** An LED or a
    raised flag on the unit, driven by the printer service off its own delivery
    count. No contact details, no channel, no consent, no PII, nothing to leak —
    and it is what a real mailbox already does. Worth building before any
    digital channel, and worth naming as the privacy-preferred default.
  - **Content must never appear in the notification.** The cloud cannot read the
    document even if it wanted to, so "you have mail" is the ceiling of what a
    server-generated notification can say. That is a *feature* to state
    explicitly, not a limitation to apologise for.
  - **Metadata is the real leak.** Even "you have mail from Acme Legal" is a
    disclosure — to whoever holds the recipient's phone, to the notification
    provider (APNs/Twilio/SMTP), and to anyone who compromises the channel.
    Default to sender-anonymous; make naming the sender an explicit per-job
    sender opt-in *and* recipient opt-in.
  - **Delivery worker.** Consume the existing Redis Stream rather than bolting
    onto the printer-link hub. The stream is at-least-once, so dedupe on
    `job_id` — a redelivered event must not notify twice.
  - **PII.** Contact details are `pgcrypto`-encrypted like the existing recipient
    columns; the notification worker needs the app key, which widens the blast
    radius of that key by one more service. Worth weighing against giving the
    worker a narrow, purpose-scoped view.
  - **Consent and abuse.** Opt-in, per-channel, with a working unsubscribe;
    rate-limit per recipient so bulk senders cannot use notifications as a
    harassment vector.

- **References.** `plans/08-data-models.md` (`recipients`, PII encryption);
  `plans/04-printer-microservice.md` (delivery status callback);
  `docs/study/07-pgcrypto-pii-encryption.md`,
  `docs/study/14-redis-streams-consumer-groups.md`.

---

## Protecting document content when the print fails (jams, spool, and the "delivered" lie)

- **What.** Close the gap between "the cryptography is sound" and "the paper is
  safe": handle a jam, a paper-out, or a mid-print failure without leaving
  plaintext recoverable — in the CUPS spool, in swap, in RAM, or as half-printed
  sheets sitting in the paper path.

- **Why V2, not V1.** V1 proves the *cryptographic* property: plaintext exists
  only in printer RAM and `/dev/shm`, is unlinked before the status callback,
  and every buffer is zeroed on every path including errors. The remaining
  exposures are **physical and operating-system** ones that need real hardware
  (a printer that can actually jam) and a locked enclosure to test against.

- **Current V1 state — including two honest gaps.**
  - `processJob` (`services/printer/print.go`) writes plaintext to
    `/dev/shm/automail-<job_id>.pdf` at 0600, prints, then unlinks, with
    `defer zeroBytes(...)` on the ciphertext, the unwrapped AES key and the
    plaintext PDF, plus a `runtime.GC()` hint that runs last. The print-failure
    path removes the tmpfs file before returning the error. That part is solid.
  - **Gap 1 — CUPS makes its own disk copy.** `printDocument` shells out to
    `lp`, which hands the file to `cupsd`; cupsd copies it into `/var/spool/cups`,
    which is **disk-backed**. Unlinking our `/dev/shm` file does not touch that
    copy. The plaintext-never-hits-disk invariant currently holds for *our*
    code and is broken by the spooler underneath it.
  - **Gap 2 — `delivered` means "CUPS accepted it".** `lp` returns when the job
    is queued, not when it is printed. So the printer reports `delivered`, wipes
    everything, and *then* the physical print happens — a jam after that point
    is invisible to the whole system, and the sender has already seen a green
    tick.

- **Sketch / considerations.**
  - **Fix the spool first.** `PreserveJobFiles No` + `PreserveJobHistory No` in
    `cupsd.conf`, and mount `/var/spool/cups` on tmpfs on the field unit. Then
    assert it: a test that greps the spool after a print, in the same spirit as
    the existing security-invariant guards.
  - **Report from IPP, not from `lp`'s exit code.** Poll the CUPS job state
    (`ipptool`/`cups` API) to a terminal state and derive `delivered` from
    `job-state = completed`, `failed` from `aborted/canceled`. That turns
    Gap 2 into a real delivery signal and makes jams observable.
  - **Retry by re-decrypting, never by retaining.** The temptation on a jam is
    to keep the plaintext around for the retry. Don't — keep the *ciphertext*
    (the cloud already retains the blob until terminal) and re-run the whole
    decrypt pipeline. Retention window stays at "one print attempt", which is
    the property worth defending.
  - **Bound the plaintext's lifetime in wall-clock, not just in control flow.**
    An unattended jam must not leave a resident buffer indefinitely: a hard TTL
    with a guaranteed zeroing path, so a hung print is wiped even if nothing
    returns.
  - **Swap is a plaintext-to-disk path.** RAM plaintext can be paged out.
    Disable swap on field units (or encrypt it), and consider `mlock` on the
    plaintext buffer so the kernel cannot page it — noting `mlock`'s limits
    (RLIMIT_MEMLOCK, and it does not survive a hibernate image).
  - **The pages themselves.** Half-printed sheets in the paper path are outside
    the threat model the crypto covers. The mitigations are physical: an
    operator-only locked enclosure, a jam that latches the unit into a `failed`
    state requiring an authenticated clear, and an audit entry for every
    physical intervention.
  - **The printer's own memory.** Many office printers retain the last job (or
    have an internal disk). A field unit should use a model without persistent
    job storage, or one whose storage can be wiped — and this should be stated
    as a hardware selection constraint, not hand-waved.
  - **Failure must stay non-informative on the wire.** The existing generic
    `"processing failed"` message must survive this work — a jam-vs-decrypt-error
    distinction leaked to the sender would reopen the oracle the current code
    deliberately closes.

- **References.** `plans/02-security.md` (plaintext lifetime);
  `plans/04-printer-microservice.md` (pipeline, DEV_MODE, status callback);
  `services/printer/print.go`, `services/printer/security_invariants_test.go`.

---

## Richer request-path observability (beyond the `X-Automail-Node` header)

- **What.** Grow the demo-grade "which node served this?" affordance into real
  request-path visibility: a per-request trace/correlation id threaded from the
  portal through the cloud node, the Redis stream and the printer link, surfaced
  in the ops dashboard.

- **Why V2, not V1.** V1 now ships the cheap version — every cloud response
  carries `X-Automail-Node`, and the portal shows which node handled a
  submission — which is enough to *demonstrate* the multi-node backend. Actual
  distributed tracing (OpenTelemetry, span propagation across the dispatch
  fan-in, a collector to store it) is a whole extra subsystem.

- **Current V1 state.** `NODE_ID` (defaulting to `$HOSTNAME`) already names each
  node as its Redis Streams consumer. A `nodeHeader` middleware stamps it on
  every HTTP response; `proxyJSON` forwards it; the portal displays the nodes
  that handled a submission. There is no correlation id and no cross-hop
  propagation.

- **Sketch / considerations.**
  - Accept/generate `X-Request-Id` at the portal proxy, log it on every hop, and
    carry it into the dispatch frame so a job can be followed cloud → stream →
    printer → status callback.
  - **The header is a topology disclosure.** A node name is not a secret, but it
    tells an attacker how many nodes there are and how requests are balanced.
    For a non-demo deployment, gate `nodeHeader` behind an env flag (default
    off) or emit an opaque per-boot id rather than the hostname.
  - The ops dashboard is the natural place to surface per-node counters
    (jobs dispatched, printer links held) — it already polls admin endpoints.

- **References.** `plans/03-scaling.md` (N stateless nodes, consumer names);
  `plans/05-cloud-server.md` (`NODE_ID`); `plans/07-ops-dashboard.md`;
  `services/cloud/middleware.go`, `services/portal/lib/proxy.ts`.

---

## More V2 ideas (unfleshed)

Drop quick one-liners here as they come up; flesh them into full sections above
when they get closer to real work.

- _(add ideas here)_
