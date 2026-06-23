# AI Collaboration Log

## Working Relationship

Claude operates as an experienced engineer in a drafter role. It brings domain knowledge — cryptographic primitives, Go patterns, distributed systems conventions — and uses it to advise, surface gaps, and draft the details that don't require the author's time. Real decisions, real work, and implementation stay with the author. Claude does not implement unless asked; it teaches first and drafts after.

Analogy: a senior drafter under a lead engineer. The drafter knows the standards cold and will flag a bad call, but the lead owns the design and does the engineering work. The drafter handles the paperwork fast so the lead's time goes toward what actually requires judgement.

For fresh sessions: respond at a tutor level. Explain the why behind decisions, not just the what. Leave implementation to the author.

**Proposal status tags used below:**
- `[accepted]` — author engaged with the proposal and agreed
- `[questioned]` — author pushed back or asked for clarification; outcome noted
- `[blind]` — author did not scrutinise this; flag for self-review

**Interview flashcards:** lines prefixed `↳ **Recall:**` are one-line answers to the questions each entry raises — study prompts to keep answers fresh on revision, not part of the design itself. Refresh them as understanding deepens.

---

## Phase 1 — System Design and Planning

### Author's contributions (take full credit)
- Defined the problem: automated mail delivery, retrofitted mailbox banks, on-site printer units
- Identified Canada Post as the B2B target
- Set the zero-knowledge constraint as the central design requirement
- Decided scope tiers (CORE / SIMPLE / STRETCH)
- Drove design gaps into view through questioning (dispatch model, guest tracking, field deployment)
- Chose to keep the printer internal for the prototype and document the production gap
- Probed the security/scaling model in depth (later session): walked through every `[blind]` item, the encryption-stack symmetry (AES symmetric vs RSA asymmetric), JWT-for-auth vs encryption-for-documents, stateless / "in-process state" semantics, and push-vs-poll transport economics
- Reversed the field-deployment call: directed implementing the dial-out dispatch model in the demo, and set the real production scale (12M mailbox units, ~30M people, power-efficiency-first)
- Set terminology: the physical unit is a **mailbox** (`mailbox_id`); the printer microservice is the printing software that runs on it

### Claude's proposals

**Hybrid AES-256-GCM + RSA-OAEP encryption scheme** `[accepted]`
Proposed by Claude; author asked to be taught the full encryption stack from the ground up (session 2). Covered: why symmetric alone fails (key distribution), why asymmetric alone fails (size limit), why GCM over CBC (authenticated encryption), why OAEP over raw RSA (randomised padding, closes Bleichenbacher attack), why 4096-bit (keys don't rotate often; compromise exposes all historical jobs for that printer), why 12-byte IV (NIST recommendation for GCM efficiency).
↳ **Recall:** *AES = **symmetric*** (one shared key; fast; no real size limit — but how do both sides get the key?). *RSA = **asymmetric*** (public/private keypair; solves key distribution; slow, and can only encrypt data smaller than the key). Hybrid = **RSA (asymmetric) ships the AES key, AES (symmetric) ships the document.** GCM vs CBC: GCM is *authenticated* (detects tampering, AEAD); CBC only hides. OAEP vs textbook RSA: randomised padding → same input gives different ciphertext, closing the Bleichenbacher padding-oracle. IV = 12 bytes because that's GCM's native nonce size (no extra hashing step).

**tmpfs + in-memory zeroing for secure wipe** `[accepted]`
Explained in the same session. Author understands why `/dev/shm` never touches disk and why explicit zeroing matters despite Go's GC (memory dump window between collection cycles).
↳ **Recall:** `/dev/shm` is a tmpfs — RAM-backed, never written to physical disk, so plaintext can't be recovered from storage. Zeroing still matters because Go's GC frees on its own schedule: until then the plaintext lingers in RAM and could surface in a core dump.

**pgcrypto for PII encryption at rest** `[accepted]`
Covered in the encryption walkthrough. Author understands the tradeoff: pgcrypto keeps logic in the DB but the app_key travels in the SQL call; application-layer encryption keeps the key out of queries entirely.
↳ **Recall:** pgcrypto encrypts/decrypts *inside* Postgres, so the `app_key` is passed into the SQL statement (visible to the DB and any query log). App-layer encryption keeps the key in the service and lets only ciphertext touch the DB. This protects *PII at rest* — it is separate from the document E2EE, which the cloud never decrypts at all.

**JWT RS256 over HS256** `[accepted]`
Explained in the same session. Author understands: HS256 requires every node to hold the shared secret; RS256 lets nodes verify with the public key only, keeping the signing key in one place. Relevant to the multi-node cloud server design. **Author probed further (later session):** what the key is/does, and confirmed the separation — JWT (RS256) is *authentication* (who are you), wholly separate from the document *encryption* (AES+RSA, confidentiality the cloud can't break); the two RSA keypairs (JWT-signing vs mailbox E2EE) are unrelated despite sharing the algorithm.
↳ **Recall:** *RS256 = **asymmetric*** signing (private key signs, public key verifies) — same RSA family as the encryption above. *HS256 = **symmetric*** (one shared secret both signs and verifies). With N nodes, RS256 means only the issuer holds the signing key while every node verifies with the public key; HS256 would force the secret onto every node (bigger blast radius if one leaks).

**Refresh token stored as a hash** `[accepted]`
Covered in the session. A DB dump yields useless hashes, not live tokens.
↳ **Recall:** Store only the hash of the refresh token; compare hashes on use. A stolen DB dump can't be replayed because hashes aren't valid tokens. (Access JWTs aren't stored at all — they're stateless and short-lived, ~15 min.)

**Redis Streams with consumer groups for job dispatch** `[questioned]`
Originally drafted blind; **author requested a full walkthrough (later session)** plus external resources. Outcome: consumer groups give at-least-once delivery — a job is claimed by one consumer, held in the PEL, and acknowledged after processing — vs plain Pub/Sub (broadcast to all subscribers, no acknowledgement, no replay). No longer blind.
↳ **Recall:** Consumer group = each job goes to *one* consumer, sits in the Pending Entries List (PEL) until `XACK`, and is replayable (`XCLAIM`/`XAUTOCLAIM`) if that consumer crashes. Pub/Sub = broadcast to *all* subscribers, no ack, no replay, lost if a subscriber is offline. Honest caveat: this is really *at-least-once delivery + idempotent processing*, not literal exactly-once.

**SELECT FOR UPDATE NOWAIT for double-dispatch prevention** `[questioned]`
Originally blind; **author reviewed (later session).** Outcome: when two nodes race for the same job, `NOWAIT` gives one the lock and the other an immediate error instead of a hang. It's the authoritative double-finalize guard alongside Redis Streams (which only decides *which node* picks up the job). No longer blind.
↳ **Recall:** Redis Streams decides which *node* picks up a queued job; the Postgres row lock is the authoritative guard that two nodes don't both finalize the *same* job (belt-and-suspenders, since cache and stream state can briefly disagree). `NOWAIT` = the loser gets an instant error and moves on instead of blocking a connection. Alternative `SKIP LOCKED` silently takes the next free row — be ready to say why you'd choose NOWAIT (fail fast / surface contention) over it.

**MinIO SSE-S3 as a second encryption layer** `[questioned]`
Originally blind; **author reviewed (later session).** Outcome: not redundant because the two layers defend different threats with independent keys — E2EE (browser key) hides contents even from the operator; SSE-S3 (MinIO-managed key) protects objects at rest against stolen disks/backups. Caveat noted: SSE-S3 buys little against an attacker who owns the running MinIO process. No longer blind.
↳ **Recall:** Not redundant because the two layers defend *different threats with independent keys*. E2EE (browser-held key) hides contents even from the cloud/MinIO operator. SSE-S3 (MinIO-managed key) protects objects at rest against stolen disks or backups. Honest caveat: SSE-S3 buys little against an attacker who owns the *running* MinIO process — its value is offline/at-rest theft.

**Audit log immutability via Postgres trigger** `[questioned]`
Originally blind; **author reviewed (later session).** Outcome: the trigger raises an exception on any UPDATE/DELETE against `audit_events`, enforced inside the DB engine below the app — so a compromised app or a direct `psql` session still can't tamper. Caveat noted: a superuser can disable the trigger, so true immutability also wants privilege separation + hash-chaining. No longer blind.
↳ **Recall:** Stronger because it's enforced *inside the DB engine, below the app* — a compromised app or a direct `psql` session still can't UPDATE/DELETE `audit_events`. App-level checks live in code an attacker may now control. Caveat: a superuser/table owner can disable the trigger, so true immutability also wants privilege separation (REVOKE UPDATE/DELETE from the app role) and, for tamper-*evidence*, hash-chaining each row to the previous.

**NAT problem for field-deployed printers** `[accepted]`
Raised by Claude unprompted. Author engaged, understood the problem, then made the call to keep the printer internal and document the production gap. WebSocket inversion solution proposed by Claude and accepted into the plans as a documented-but-not-implemented consideration. **Superseded — see below.**

**Dispatch inversion implemented in the demo (printer dials out)** `[questioned]`
Author revisited the earlier "keep it internal, document the gap" decision and proposed implementing the WebSocket inversion in the demo itself, reasoning it would make the prototype mirror production. Claude pushed back on one point in the original framing: the inversion is not merely "a networking problem" as 01-architecture.md had claimed — in a multi-node cloud server it is a genuine distributed-systems problem (delivering to a stateful connection held on one node of a stateless cluster), and is the mirror image of the SSE fan-out already solved. Author agreed and directed the rework. Outcome: the printer is now a dial-out WebSocket client; the cloud server holds a per-node connection registry and routes dispatch across nodes via a Redis `mailbox:<id>:dispatch` channel. Author drove the decision; Claude drafted the plan changes across 01/03/04/05/09/10. **Author should self-review** the cross-node routing and the "recoverable soft state" framing in 03-scaling.md — these were drafted, not yet scrutinised in depth.
↳ **Recall:** The symmetric pair to internalise — *SSE = **fan-out*** (one status event → the node holding the *sender's* connection) and *printer link = **fan-in*** (a dispatch from any node → the node holding the *printer's* connection). Both bridged by the same Redis pub/sub. The mTLS direction is unchanged (printer still presents the cert); only the *dial* inverts. And "no in-process state" becomes "no *authoritative* in-process state" — the held socket is recoverable soft state (printer reconnects elsewhere; job state stays in Postgres/Redis).

**Push vs poll dispatch transport (power vs latency at scale)** `[accepted]`
Author challenged the persistent connection on efficiency grounds and set the real target: ~12M shared-mailbox units, ~30M people, latency-tolerant, **power efficiency = primary operating-cost lever**. Claude's analysis: a held mTLS socket is cheap *when idle* and beats polling on CPU/bytes for always-on mains-powered hardware — but at 12M units the server-side cost of holding sockets and the per-device radio/CPU power make **jittered polling the right production model** (with TLS session resumption to avoid repeat asymmetric handshakes). Decision: document polling as the production model; keep the demo on persistent push for low latency + the cross-node fan-in showcase; make transport a swappable `DISPATCH_MODE = push | poll` layer over a shared pipeline.
↳ **Recall:** Persistent socket = low latency, but holding 12M sockets + always-on radios is the opex killer. Polling = wake/check/sleep saves power at scale; latency (minutes) is fine for mail. Same job/queue/crypto pipeline underneath; only the delivery hop swaps. Idle persistent connection is *cheaper per-message* than polling (no repeat handshake) — polling wins only on *power* and *server statelessness*, which is what matters at 12M units.

**Production Considerations section in `01-architecture.md`** `[accepted]`
Proposed and written by Claude after the NAT discussion. Author reviewed and approved the content without changes.

---

**Asset management & field maintenance (`12-asset-management.md`)** `[accepted]`
Author identified a practical operating need: at 12M units, mail delivery depends on physical hardware, so the system must push diagnostics to maintenance crews and dispatch field technicians on fault. Claude drafted the component — telemetry riding the existing dispatch transport (no new connection), a fault rule engine opening maintenance tickets, and an Automail-team fleet console distinct from the property-manager ops dashboard. Author defined the need and scale; Claude drafted.
↳ **Recall:** Reuses the same device channel as job dispatch (diagnostics block on `state` frames in push mode / piggybacked on polls in poll mode) — so it adds *no* extra power/network cost per unit. Distinct from the ops dashboard (07 = one building, job metadata; 12 = whole fleet, physical health + maintenance workflow). Diagnostics are operational metadata only — same zero-knowledge boundary, no PII or document bytes.

---

*Updated as implementation begins. Each entry will note what Claude generated, what the author changed, and what was solved in practice.*
