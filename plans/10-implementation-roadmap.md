# Implementation Roadmap

Each phase has a clear deliverable you can verify before moving on. Security infrastructure comes first — every subsequent phase builds on top of it.

---

## Phase 0 — Foundation

**Goal**: Docker Compose stack running; Traefik routes to cloud server; health check passes.

Tasks:
- `git init`, `.gitignore` (exclude `.env`, `*.pem`, `certs/`)
- Directory structure:
  ```
  services/
    cloud/
    printer/
    portal/
  infra/
    certs/        (generated, git-ignored)
    postgres/
  docker-compose.yml
  .env.example
  ```
- `go mod init` in `services/cloud/` and `services/printer/`
- `npx create-next-app@latest services/portal`
- `docker-compose.yml`: Postgres, Redis, MinIO, Traefik, cloud-server stub, portal stub
- Cloud server: `GET /healthz` returns `200 OK`
- Traefik: routes `api.automail.local` → cloud server; verify round-robin with `--scale cloud-server=2`

**Verify**: `curl https://api.automail.local/healthz` returns `{"status":"ok"}` with two cloud server containers running.

---

## Phase 1 — Security Infrastructure

**Goal**: mTLS certs generated; MinIO SSE enabled; JWT keypair ready; Postgres schema with pgcrypto; audit trigger in place.

Tasks:
- `infra/certs/gen.sh`: openssl script generating internal CA, cloud-server cert, printer-service cert
- Add cert paths to `.env.example`
- MinIO: configure SSE-S3 via environment variables in `docker-compose.yml`
- Generate RS256 JWT keypair: `openssl genpkey -algorithm RSA ...`
- Generate printer RSA-4096 keypair (encrypted at rest)
- `services/cloud/db/schema.sql`: all tables from [08-data-models.md](08-data-models.md)
- Run `sqlc generate`
- `pgcrypto` extension enable in schema
- Audit immutability trigger
- Postgres migration runner (simplest option: `golang-migrate` or just run schema.sql at startup in dev)

**Verify**: `psql -c "\dt"` shows all tables; trigger prevents `DELETE FROM audit_events`; mTLS `curl --cert ... --key ...` to a test endpoint succeeds.

---

## Phase 2 — Cloud Server Core

**Goal**: Recipient lookup works; guest job submission stores ciphertext; auth infrastructure is in place but not required by the portal yet.

Tasks:
- `GET /recipients` — search by name/address (no auth)
- `GET /recipients/:id/public-key` — resolve recipient to printer public key (no auth)
- `POST /jobs/upload-url` — MinIO pre-signed PUT URL generation (auth optional)
- `POST /jobs` — resolve recipient → slot + printer, insert job (`sender_id = NULL` for guest, `guest_token_hash` set), write audit event
  - Skip dispatch logic for now — just queue every job
- Redis printer state cache read (stub: always return `idle` + empty slots for now)
- JWT middleware (RS256 verify) — built here, enforced only on admin and account endpoints
- `POST /auth/login`, `POST /auth/refresh`, `POST /auth/logout` — built here, used in Phase 8

**Verify**: `POST /jobs` with no auth header inserts a row with `sender_id = NULL`; response includes `guest_token`; audit event written.

---

## Phase 3 — Printer Microservice

**Goal**: Printer service starts; dials out and registers; cloud server caches its state; dev-mode print works.

Tasks:
- mTLS WebSocket client: dial `wss://cloud-server:8443/internal/printer-link` (present client cert), reconnect with exponential backoff + jitter
- On connect: send `register` frame `{ mailbox_id, slot_occupancy: {slot-1: {0,5}} }`
- Keepalive: WebSocket ping/pong every 30s; send a `state` frame on change (replaces heartbeat POST)
- Cloud side: `GET /internal/printer-link` upgrade handler; add socket to per-node registry; subscribe node to `mailbox:<id>:dispatch`; seed Redis `mailbox:<id>:state`
- `dispatch` frame handler on the mailbox:
  - Fetch blob from `blob_url` (HEAD request to verify URL is valid in dev)
  - In dev mode: skip RSA/AES decrypt; write a dummy file to `/tmp/automail-dev-<job_id>.txt`; delete immediately
  - Send `status` frame back up the socket: `{ job_id, status: delivered }`
- `GET /healthz` (local) for the printer container healthcheck

**Verify**: Cloud server logs the printer registering; `mailbox:<id>:state` appears in Redis; publishing a test message to `mailbox:<id>:dispatch` reaches the printer and produces a `status` frame; job status updates to `delivered` in Postgres.

---

## Phase 4 — Real Dispatch + Queue

**Goal**: Immediate dispatch works; blocked jobs queue via Redis Streams; heartbeat-triggered re-dispatch works.

Tasks:
- Cloud server: `tryDispatch()` — check Redis printer state, `SELECT FOR UPDATE NOWAIT`, then `PUBLISH mailbox:<id>:dispatch` (route to the owner node's socket); re-queue if zero subscribers
- Owner node `pumpDispatch`: Redis message → write `dispatch` frame down the printer's WebSocket
- On dispatch failure (not idle, slot full, or offline): `XADD` to `jobs:pending` Redis Stream
- Consumer group setup on startup
- Dispatcher goroutine: `XREADGROUP`, attempt dispatch, `XACK` on success
- `state`-frame handler: update Redis printer state cache; `PUBLISH mailbox:<id>:available`
- Dispatcher subscribes to `mailbox:<id>:available`; re-evaluates stream on each event
- `XAUTOCLAIM` periodic sweep for crash recovery (covers owner-node failover)

**Verify**: Submit job while printer is "busy" (manually set Redis state) → job queued; update Redis state to idle → job dispatches within heartbeat interval.

---

## Phase 5 — SSE + Status Relay

**Goal**: Job status changes are pushed to the sender's browser in real time.

Tasks:
- `status`-frame handler (on the printer-link read loop): update Postgres job status; `PUBLISH job:<id>:status` to Redis
- `GET /jobs/:id/stream` handler: subscribe to Redis pub/sub channel; write SSE events
- Test cross-node both ways: with two cloud nodes, confirm a job claimed on the node that does **not** hold the printer socket still dispatches (dispatch fan-in), and a status update still reaches an SSE client on a third connection (status fan-out)

**Verify**: Open `/jobs/:id/stream` in browser; trigger a job delivery; browser receives `{"status":"delivered"}` without polling.

---

## Phase 6 — Full Crypto (Printer Microservice)

**Goal**: Real RSA+AES decryption in the printer microservice. Replaces the Phase 3 stub.

Tasks:
- Load RSA private key at startup (encrypted PEM + passphrase env var; zero passphrase after use)
- `crypto.go`: `DecryptAESKey(encryptedKey []byte) ([]byte, error)` — RSA-OAEP decrypt
- `crypto.go`: `DecryptDocument(ciphertext, aesKey []byte) ([]byte, error)` — AES-256-GCM, parse IV from first 12 bytes
- tmpfs write to `/dev/shm/automail-<job_id>.pdf`
- `lp -d $PRINTER_NAME /dev/shm/automail-<job_id>.pdf` (or dev-mode skip)
- `os.Remove(tmpfsPath)` immediately after CUPS accepts
- `zeroBytes()` on all sensitive slices; `runtime.GC()`

**Verify**: Upload a real encrypted PDF via the portal (Phase 7 will be needed for the UI, but can test with `curl` + the Web Crypto API in browser console); printer logs show job printed; no PDF remains in `/dev/shm` after delivery.

---

## Phase 7 — Guest Sender Portal

**Goal**: Full browser-based encrypt-upload-submit flow for guests; live status display using guest token. No login required.

Tasks:
- `lib/encrypt.ts`: `encryptDocument()` — AES-256-GCM + RSA-OAEP (see [06-sender-portal.md](06-sender-portal.md))
- Recipient search form: type address/name → select from results
- Job submission form: select PDF, encrypt, upload to MinIO, POST job (no auth)
- Display `guest_token` to user after submission with a prompt to save it for tracking
- `/track` page: input `job_id` + `guest_token` → open SSE stream, display status transitions
- Next.js API routes as thin proxies to cloud server

**Verify**: Open portal in browser as a guest; upload a PDF; receive a guest token; use it on the track page to watch status change from `submitted` → `dispatching` → `printing` → `delivered` in real time.

---

## Phase 8 — Sender Accounts

**Goal**: Optional login for senders who want persistent job history without re-entering a guest token.

Tasks:
- Login / register pages (email + password → JWT stored in `HttpOnly` cookie)
- Authenticated `POST /jobs` path: job stored with `sender_id` set, no `guest_token` issued
- `/history` page: list of the authenticated sender's past jobs and their statuses
- `/jobs/:id` page: SSE stream with JWT ownership check (replaces guest token check when logged in)
- JWT auth middleware in Next.js (redirect to login only for account pages — guest flow still works unauthenticated)

**Verify**: Register an account; submit a job while logged in; see it in `/history`; log out; submit a second job as guest; confirm it does not appear in any account history.

---

## Phase 9 — Ops Dashboard

**Goal**: Admin can see job queue and printer status.

Tasks:
- `/admin` page: job queue counts, printer status summary
- `/admin/jobs` page: job table with status filter
- `/admin/mailboxes` page: per-mailbox status + slot occupancy text list
- Admin JWT role check middleware

**Verify**: Log in as admin; see live job list and printer status; submit a job as sender and watch it appear in admin job list.

---

## Phase 10 — Real CUPS Integration [STRETCH]

**Goal**: Replace dev-mode file write with actual printing to the home printer.

Tasks:
- Configure CUPS on the Proxmox VM host; share the home printer
- Mount CUPS socket into the printer-service container (or configure TCP CUPS)
- Replace `DEV_MODE` stub with real `lp -d $PRINTER_NAME ...` call
- Test: submit a job, confirm physical print comes out

**Verify**: Paper comes out of the printer with the correct document content.

---

## Reference Order

Each phase builds on the previous. Do not skip phases — the security infrastructure (Phase 1) must exist before any business logic is implemented.

```
0 (Foundation)
└── 1 (Security infra)
    └── 2 (Cloud server core — guest endpoints + auth infrastructure)
        ├── 3 (Printer microservice stub)
        │   └── 4 (Dispatch + queue)
        │       └── 5 (SSE relay)
        │           └── 6 (Real crypto in printer)
        │               └── 7 (Guest sender portal)
        │                   └── 8 (Sender accounts — optional login + history)
        │                       └── 9 (Ops dashboard)
        │                           └── 10 [STRETCH] (Real CUPS)
        └── (Phase 3 can start in parallel with Phase 2 once schema is done)
```
