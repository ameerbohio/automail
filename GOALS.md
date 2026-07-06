# Automail Development Goals

This document drives autonomous, sequential development of the remaining roadmap
phases. It is designed to be executed by an agent via a single recurring goal prompt:

> Read GOALS.md in the automail repo. Find the first goal whose Status is
> `pending`, execute it following the Process Rules, verify its acceptance
> criteria, mark it `done` (update its Status line and the Status Log), then
> continue to the next goal. Stop when you hit a goal marked `blocked-on-owner`,
> when a goal's acceptance criteria cannot be verified, or when all goals are done.

## Process Rules (apply to every goal)

1. **Source of truth**: `plans/` is the specification. Read the relevant plan
   before implementing. `plans/10-implementation-roadmap.md` defines each phase's
   "Verify:" line — that is the definition of done.
2. **Workflow per phase**: implement with the `phase-implementer` agent → verify
   with the `plan-checker` agent → fix any drift → commit. One phase per commit,
   clean subject + body, **no AI co-author trailer**.
3. **Security invariants (non-negotiable, check before every commit)**:
   - The cloud server never decrypts, logs, or forwards `encrypted_key` anywhere
     but the printer link. Zero-knowledge is the point of the project.
   - Plaintext exists only in printer RAM + tmpfs (`/dev/shm`), is unlinked
     before the status callback, then zeroed. Never on disk, in logs, or on the
     network.
   - mTLS on every internal hop, including the printer's dial-out WebSocket.
   - Secrets (`.env`, `*.pem`, `certs/`) are gitignored — never commit them.
4. **Study docs are a deliverable**: every non-trivial concept implemented gets a
   short interview-oriented explainer under `docs/study/` (see its README).
   Side questions that come up go into
   `docs/study/00-interview-pending-questions.md`, not answered in-session.
5. **Verification is real**: run `go build ./... && go vet ./... && go test ./...`
   in both `services/cloud` and `services/printer` (and portal lint/build once it
   has real code) before marking any goal done. Exercise the phase's "Verify:"
   line end-to-end where the environment allows (Docker Compose stack).
6. **Do not skip or reorder goals.** Each phase builds on the previous.
7. **Stopping conditions**: a goal marked `blocked-on-owner`, unresolvable
   plan-checker findings, failing acceptance criteria, or any action that is
   destructive/irreversible. Record the blocker in the Status Log and stop.
8. **After completing a goal**: flip its Status to `done`, append a Status Log
   entry (date, commit hash, one-line outcome), and move on to the next goal.

---

## Goal 0 — Land the outstanding review fixes

**Status:** done

Commit the uncommitted working-tree changes in `services/cloud`:
- `dispatch/dispatcher.go` — `XReadGroup Block: -1` fix (Block: 0 sends `BLOCK 0`
  = wait forever in go-redis v9, which pinned the dispatcher goroutine and starved
  the retry/sweep paths) plus the `TestDispatcher_DrainReturnsOnEmptyStream`
  regression test.
- `handlers/recipients.go` — `maskName` first-initial now rune-safe
  (`utf8.DecodeRuneInString` instead of byte slicing).
- `middleware.go` — stale `requireAuth` comment corrected.

Run build/vet/test in both Go modules first. Commit with a clean subject + body
explaining the BLOCK 0 semantics bug. Do **not** bundle the untracked `docs/`
files (`docs/development-process.md`, `docs/study/00-*.md`, `docs/study/16-*.md`)
or this `GOALS.md` into that commit — commit GOALS.md separately if desired; the
owner's untracked docs are left alone.

**Acceptance:** working tree clean apart from the owner's untracked docs; both
modules build, vet, and test green; `git log -1` shows the fix commit.

---

## Goal 1 — Phase 5: SSE + Status Relay

**Status:** done

Implement exactly Phase 5 of `plans/10-implementation-roadmap.md`. Scope:

- `GET /jobs/:id/stream` SSE handler subscribing to the `job:<id>:status` Redis
  pub/sub channel (the printer-link hub already publishes there).
- Auth: JWT ownership check (authenticated sender must own the job) **or**
  `?token=<guest_token>` verified against `jobs.guest_token_hash`
  (`plans/09-api-contracts.md`).
- The internal pub/sub payload (`link.statusPayload`) deliberately omits
  `job_id` — the stream handler must add it back into each SSE `data:` line to
  match the documented wire format.
- Connection closes when status reaches `delivered` or `failed`.
- Test cross-node fan-out with two cloud-server replicas: a status update must
  reach an SSE client connected to a node that does not hold the printer socket.

**Acceptance (roadmap Verify):** opening `/jobs/:id/stream` and triggering a job
delivery pushes `{"job_id":...,"status":"delivered"}` to the client without
polling. Study doc: SSE vs WebSocket choice + Redis pub/sub fan-out. One commit.

---

## Goal 2 — Phase 6: Full Crypto in the Printer

**Status:** done

Implement exactly Phase 6 of `plans/10-implementation-roadmap.md`. Scope:

- Printer loads its RSA private key at startup from encrypted PEM + passphrase
  env var; zero the passphrase after use.
- `crypto.go`: `DecryptAESKey` (RSA-OAEP) and `DecryptDocument` (AES-256-GCM,
  IV = first 12 bytes of ciphertext).
- Plaintext written **only** to `/dev/shm/automail-<job_id>.pdf`.
- `lp -d $PRINTER_NAME` call (or dev-mode skip).
- `os.Remove` the tmpfs file **before** sending the `delivered` status frame.
- `zeroBytes()` on every sensitive slice; `runtime.GC()` after.
- The cloud server side must not change in any way that sees or logs
  `encrypted_key`.

**Acceptance (roadmap Verify):** an encrypted PDF submitted end-to-end is
processed (dev-mode logs show the pipeline); nothing remains in `/dev/shm`
afterwards. Study doc: extend `docs/study/16-hybrid-encryption.md` (already
exists — do not duplicate) with the memory-hygiene story. One commit.

---

## Goal 3 — Phase 7: Guest Sender Portal

**Status:** pending

Implement exactly Phase 7 of `plans/10-implementation-roadmap.md`. Scope
(Next.js app in `services/portal`, currently a stub):

- `lib/encrypt.ts`: `encryptDocument()` — AES-256-GCM + RSA-OAEP via Web Crypto,
  per `plans/06-sender-portal.md`.
- Recipient search form (name/address → select from results).
- Job submission: select PDF → encrypt in browser → PUT to pre-signed MinIO URL
  → `POST /jobs` with no auth.
- Show the `guest_token` exactly once with a save-it prompt.
- `/track` page: `job_id` + `guest_token` → open the SSE stream, display status
  transitions live.
- Next.js API routes are thin proxies only. The plaintext PDF and the raw AES
  key must never leave the browser.

**Acceptance (roadmap Verify):** full guest flow in a browser — upload a PDF,
receive a guest token, watch status go `submitted` → `dispatching` → `printing`
→ `delivered` live on `/track`. Study doc: Web Crypto E2EE flow and why the
server never sees key material. One commit.

---

## Goal 4 — Phase 8: Sender Accounts

**Status:** pending

Implement exactly Phase 8 of `plans/10-implementation-roadmap.md`. Scope:

- Register/login pages using the existing Phase 2 auth endpoints (HttpOnly
  refresh-cookie flow already exists server-side; a register endpoint may need
  to be added — check `plans/09-api-contracts.md` first).
- Authenticated `POST /jobs` path stores `sender_id`, issues no guest token
  (server already does this; wire the portal to send the Bearer token).
- `/history` page: the authenticated sender's jobs and statuses.
- `/jobs/:id` page: SSE stream gated by JWT ownership instead of guest token.
- Next.js middleware redirects to login for account pages **only** — the guest
  flow must keep working unauthenticated.

**Acceptance (roadmap Verify):** register; submit a job logged-in; see it in
`/history`; log out; submit a guest job; confirm it appears in no account
history. One commit.

---

## Goal 5 — Phase 9: Ops Dashboard

**Status:** pending

Implement exactly Phase 9 of `plans/10-implementation-roadmap.md`. Scope:

- `/admin`: job queue counts + printer status summary.
- `/admin/jobs`: job table with status filter.
- `/admin/mailboxes`: per-mailbox status + slot occupancy list.
- Admin-role JWT check using the existing `requireAuth` middleware plus a role
  guard (`GET /admin/jobs` contract in `plans/09-api-contracts.md`).
- Admin views expose job **metadata only** — `encrypted_key` is never rendered,
  returned by any admin endpoint, or logged.

**Acceptance (roadmap Verify):** as admin, see the live job list and printer
status; a job submitted as a sender appears in the admin job list. One commit.

---

## Goal 6 — Phase 10 (stretch): Real CUPS Printing

**Status:** blocked-on-owner

Requires physical hardware and host configuration the agent must not guess at:
CUPS configured on the Proxmox VM host, the home printer shared, and the CUPS
socket (or TCP CUPS) exposed to the printer container.

When unblocked: replace the `DEV_MODE` stub with the real
`lp -d $PRINTER_NAME /dev/shm/automail-<job_id>.pdf` call, per Phase 10 of the
roadmap. Until then, an agent reaching this goal should produce a precise,
step-by-step list of the manual host setup required, write it to
`docs/cups-host-setup.md`, and stop.

**Acceptance (roadmap Verify):** paper comes out of the printer with the correct
document content; `/dev/shm` is empty afterwards.

---

## Goal 7 — Recurring Quality Gate

**Status:** recurring (run after each phase goal completes, or on a schedule —
never marked done)

Audit the repo:

1. `go build ./... && go vet ./... && go test ./...` in `services/cloud` and
   `services/printer`; portal lint/build once it has real code.
2. Run the `plan-checker` agent against the latest committed phase.
3. Verify the security invariants by inspection: no cloud-server code path
   decrypts or logs `encrypted_key`; printer plaintext confined to `/dev/shm`,
   unlinked before status, zeroed; mTLS on every internal hop; no secrets
   committed.
4. Confirm each non-trivial concept in recent commits has a `docs/study/`
   explainer, and `docs/study/00-interview-pending-questions.md` is being
   worked down, not just appended to.
5. Report findings before fixing anything — list first, fix only clear-cut
   regressions, leave design questions for the owner.

---

## Status Log

| Date | Goal | Commit | Outcome |
|------|------|--------|---------|
| 2026-07-05 | Goal 0 | 21af1f3 | Review fixes landed: XReadGroup Block:-1 (BLOCK 0 = wait forever), rune-safe maskName, requireAuth comment; both modules build/vet/test green. |
| 2026-07-05 | Goal 1 | 69d11ba | Phase 5 SSE relay: /jobs/:id/stream with dual auth, job_id restored to wire format, terminal close, two-node fan-out test. plan-checker PASS. Browser Verify deferred: Docker unavailable in session; covered by in-process cross-node test. |
| 2026-07-06 | Goal 2 | 9dedbdb | Phase 6 printer crypto: RSA-OAEP unwrap + AES-256-GCM decrypt in RAM, /dev/shm tmpfs, unlink-before-delivered, zero+GC; PBES2 PKCS#8 key load with hand-rolled PBKDF2 (no new dep), passphrase zeroed+env-unset; generic wire error (no oracle). Cloud: delete spent ciphertext on delivered (blob_ref only, never encrypted_key). plan-checker PASS (fixed GC-order nit + stale plans/04 dev-mode text). Browser/Docker E2E deferred; covered by full-pipeline unit test. |
