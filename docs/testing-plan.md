# Automail — Production-Readiness Test Plan (Local)

**Level:** Senior / SDET. This is how a security- and distributed-systems-heavy
service is hardened for production at a company that takes correctness seriously —
executed entirely on your local machine (WSL2 + Docker), no cloud required.

**How to use this doc.** Like [plans/10-implementation-roadmap.md](plans/10-implementation-roadmap.md),
it is split into **Parts** you execute one at a time. Each Part is independently
valuable, ordered so earlier ones unblock later ones. Every Part ends with a
**Verify:** line that defines "done." Do them in order; ship after each.

> **Interview framing.** Testing questions at the senior level are rarely "did you
> write tests." They are: *what did you choose not to test and why; where's the
> risk; how do you know the zero-knowledge invariant actually holds; how does this
> behave when a node dies mid-dispatch.* This plan is built to give you a crisp
> answer to each.

---

## The strategy in one picture

```
                 fewer, slower, higher-confidence
        ┌─────────────────────────────────────────┐
        │  Part 8  Load / perf (k6)                │
        │  Part 7  Resilience & chaos              │
        │  Part 5  Full-system E2E (compose)       │  <- prove the product works
        │  Part 4  Portal E2E (Playwright)         │
        ├─────────────────────────────────────────┤
        │  Part 3  Crypto contract (cross-language)│  <- the security seam
        │  Part 2  Integration (real PG/Redis/MinIO)│
        ├─────────────────────────────────────────┤
        │  Part 1  Unit + fuzz + race (Go & TS)    │  <- fast, run on every save
        └─────────────────────────────────────────┘
        Cross-cutting: Part 0 (CI/gates), Part 6 (security invariants + scanning)
                 more, faster, run constantly
```

The two cross-cutting Parts (0 = plumbing, 6 = the non-negotiable security
invariants) wrap every layer.

---

## Current state (honest baseline, 2026-07)

| Area | State | Gap |
|---|---|---|
| `services/cloud` Go unit/integration | ~1,900 lines, 30+ tests, uses **fakes** | No real-dependency integration; no `-race`; no coverage gate |
| `services/printer` Go unit | ~730 lines, crypto round-trips + OpenSSL interop + wipe checks | No fuzzing on frame/crypto parsers |
| `services/portal` (Next.js) | **No test tooling, no tests** | `encrypt.ts` (browser crypto) entirely unverified |
| Cross-language crypto | printer↔OpenSSL interop only | **Portal-encrypt → printer-decrypt path never tested end to end** |
| Full-system E2E | none | No proof the assembled product works |
| Security invariants | asserted in prose in `CLAUDE.md` / `plans/02-security.md` | **Not enforced by any test** |
| CI | none (`.github/workflows` absent) | Nothing runs automatically; no gates |
| Load / chaos | none | Unknown behavior under concurrency, restarts, node failover |

**If you only do three things:** Part 3 (crypto contract), Part 6 (security
invariants as executable guards), Part 0 (CI so the above never regress). Those
three are what separate "it works on my machine" from "I can defend this design."

---

## Part 0 — Test infrastructure & CI foundation

**Goal:** one command runs everything; a pipeline runs it on every push; regressions
become impossible to merge silently.

Tasks:
- `Makefile` targets: `test-unit`, `test-integration`, `test-e2e`, `test-race`,
  `lint`, `cover`, `fuzz`, and a `ci` target that chains the gates.
- `go test ./... -race -count=1` wired into `test-race`. The race detector is
  non-negotiable for a service with goroutines fanning WebSocket frames, Redis
  pub/sub, and SSE — this is where data races live.
- Coverage: `go test ./... -coverprofile` with a **floor** (start at the current
  number, ratchet up; never let it drop). Print per-package coverage.
- `.github/workflows/ci.yml`: matrix job — Go build+vet+race+cover for both
  services, `next lint` + `tsc --noEmit` + portal unit tests, and the integration
  suite (Part 2) using service containers. Cache Go modules and npm.
- Pre-commit hook (optional but senior-standard): `gofmt`, `go vet`, `next lint`,
  and a secret scan (Part 6) so `.env`/`*.pem` can never be committed.

**Why it matters (interview):** "coverage gate + race detector in CI" is the
single most credible signal that a codebase is maintained to a professional bar.
Be ready to explain *ratcheting* coverage (floor that only goes up) vs a fixed
target (which teams game).

**Verify:** `make ci` passes locally and the same steps pass in GitHub Actions on
a pushed branch; deliberately introducing a data race fails `test-race`; deleting a
test that drops coverage below the floor fails the build.

---

## Part 1 — Strengthen the unit layer (Go + TS)

**Goal:** the fast layer catches logic bugs and parser abuse before anything heavier runs.

Tasks:
- **Table-driven edge cases** on the pure logic already under test: dispatch
  eligibility (`TestEligible`), backoff/jitter bounds, JWT claim/role parsing,
  PKCS7 unpad, guest-token hashing. Add the *nasty* rows: empty, max-size,
  off-by-one slot indices, expired/nbf JWTs, malformed base64.
- **Go native fuzzing** on every byte-parser that touches untrusted input:
  - `link/frames.go` and `printer/frames.go` — `FuzzFrameUnmarshal`: never panic,
    never allocate unbounded on a hostile frame.
  - `printer/crypto.go` — `FuzzDecryptDocument`: random ciphertext must return an
    error, never panic, never leak past the GCM tag check.
- Seed the fuzz corpus with the real vectors you already have (OpenSSL interop
  output). Run fuzzers in CI with a short time budget; run long locally.
- **Portal unit tooling** (net-new): add **Vitest** to `services/portal`, first
  target is `lib/encrypt.ts` — covered properly in Part 3.

**Why it matters (interview):** fuzzing a frame parser and a decrypt routine is
exactly the "assume the input is adversarial" mindset a zero-knowledge system
demands. You can say you fuzz the WebSocket frame boundary because it's reachable
from the network hop.

**Verify:** `go test ./... -run xxx -fuzz Fuzz... -fuzztime=30s` finds no crashers
on either frame parser or `DecryptDocument`; new edge-case tables are green;
`vitest` runs in the portal.

---

## Part 2 — Integration tests against real dependencies

**Goal:** replace fakes with the real thing where the fake hides risk — real
Postgres (pgcrypto, the audit-immutability trigger, `SELECT FOR UPDATE NOWAIT`),
real Redis (Streams consumer groups, pub/sub, `XAUTOCLAIM`), real MinIO
(pre-signed PUT/GET, SSE).

Tasks:
- Adopt **`testcontainers-go`** (or a `docker-compose.test.yml` + build tag
  `//go:build integration`). Spin ephemeral Postgres/Redis/MinIO per suite.
- Postgres integration: schema applies clean; the **audit trigger actually blocks
  `DELETE FROM audit_events`** (this is currently prose in the roadmap Verify line —
  make it a test); `SELECT FOR UPDATE NOWAIT` returns the lock error under
  contention, not a hang.
- Redis integration: Streams `XADD`/`XREADGROUP`/`XACK` round-trip; a message left
  un-ACKed is reclaimed by `XAUTOCLAIM` (crash-recovery path); pub/sub reaches a
  subscriber on a different connection (the cross-node fan-out the design depends on).
- MinIO integration: pre-signed PUT then GET round-trips ciphertext; object is
  server-side encrypted; **the cloud path only ever handles the pre-signed URL, not
  bytes** (assert the cloud code never reads blob contents).

**Why it matters (interview):** fakes prove your code calls the right methods; they
do *not* prove Postgres honors `NOWAIT` or that your consumer group survives a
crash. Naming exactly which behaviors you promoted from fake → real is a strong
signal.

**Verify:** `make test-integration` boots the three containers, all suites pass,
and tearing a container down mid-test produces a clean, explained failure (not a
hang).

---

## Part 3 — The crypto contract (cross-language E2E) — highest priority

**Goal:** prove the security-critical seam: a document encrypted by the **browser**
(`portal/lib/encrypt.ts`, Web Crypto: AES-256-GCM + RSA-OAEP) decrypts **byte-for-byte**
in the **printer** (`printer/crypto.go`). Today each half is tested against OpenSSL,
but never against *each other* — a subtle mismatch (IV length, OAEP hash, GCM tag
placement, key wrapping order) would pass both suites and fail in production.

Tasks:
- Generate a fixed RSA keypair as a **golden fixture** (committed test key, clearly
  marked non-production).
- **Direction A (the real path):** a Node/Vitest test imports `encryptDocument()`,
  encrypts a known PDF with the fixture public key, and emits `{encrypted_key,
  iv, ciphertext}` as a JSON vector. A Go test in `printer` loads that vector and
  asserts `DecryptAESKey` + `DecryptDocument` reproduce the exact input bytes.
- **Direction B (guard):** a Go-encrypt / TS-decrypt pair to catch asymmetries, even
  though only A is used in production.
- Wire both into a `make crypto-contract` target so the vector is regenerated and
  re-verified, not stale-committed.
- Assert the **negative**: a one-bit flip in ciphertext or `encrypted_key` makes the
  printer reject (GCM auth failure / OAEP failure) — never a silent partial decrypt.

**Why it matters (interview):** this is *the* differentiating test. "I have a
cross-language golden-vector contract between the browser encryptor and the printer
decryptor, regenerated in CI, with a tamper-rejection assertion" is a senior-level
answer about testing a heterogeneous crypto boundary. It's also the bug class most
likely to actually bite you.

**Verify:** `make crypto-contract` regenerates the browser-produced vector and the
printer decrypts it to the original bytes; a tampered vector is rejected with an
auth error, never a crash or partial plaintext.

---

## Part 4 — Portal testing (unit + browser E2E)

**Goal:** the untested half of the system gets a safety net; the guest and account
UX flows are exercised in a real browser.

Tasks:
- **Vitest unit** (from Part 1 tooling): `encrypt.ts` chunking/IV handling; the thin
  Next.js API proxy routes (correct forwarding, no auth leakage, guest vs
  authenticated path selection).
- **Playwright E2E** driving a real browser against the compose stack:
  - Guest flow: search recipient → pick PDF → encrypt in-browser → upload → submit →
    receive guest token → `/track` shows `submitted → dispatching → printing →
    delivered` over SSE (roadmap Phase 7 Verify, automated).
  - Account flow: register → submit logged in → job appears in `/history`; log out;
    guest submit does **not** appear in any account history (Phase 8 Verify).
  - Admin flow: admin sees the job in `/admin/jobs`; a non-admin JWT is refused
    (Phase 9 Verify).
- Assert in the browser context that **plaintext never leaves the tab** — the
  request body to MinIO is ciphertext (inspect the intercepted upload).

**Why it matters (interview):** you can demonstrate the zero-knowledge claim from
the *client* side — "here's a Playwright assertion that the bytes on the wire to
storage are ciphertext" — which is more convincing than a diagram.

**Verify:** `make test-e2e` runs Playwright headless against `docker-compose up`;
guest, account, and admin journeys pass; the upload-body-is-ciphertext assertion holds.

---

## Part 5 — Full-system E2E (assembled product)

**Goal:** one automated test drives the entire real stack — portal → cloud → Redis
dispatch → printer decrypt → (dev-mode) print → status back to browser — proving the
seams between services, not just each service.

Tasks:
- A `docker-compose.test.yml` (or reuse the dev compose with `DEV_MODE=true`) that
  brings up traefik, postgres, redis, minio, cloud-server, printer, portal, and the
  mTLS certs.
- A driver test (Go or Playwright) that submits a real encrypted job and asserts the
  end-to-end status transition **and** the printer-side wipe: after `delivered`, no
  file remains in `/dev/shm` (extends `TestHandleDispatch_DeliversAndWipes` to the
  full stack).
- Cross-node case: run `--scale cloud-server=2`; confirm a job claimed on the node
  that does *not* hold the printer socket still dispatches (dispatch fan-in) and the
  SSE status still reaches a client on a third connection (status fan-out) — roadmap
  Phase 5 Verify, automated.

**Why it matters (interview):** this is where you prove the distributed design (owner
node vs claiming node, Redis as the fan-in/fan-out backbone) actually holds when the
pieces are real processes over mTLS, not in-process fakes.

**Verify:** `make test-e2e-full` boots the stack, runs a real encrypted job to
`delivered`, confirms `/dev/shm` is clean afterward, and passes the two-node
fan-in/fan-out case.

---

## Part 6 — Security invariants as executable guards + scanning (cross-cutting)

**Goal:** turn the non-negotiables in `CLAUDE.md` / [plans/02-security.md](plans/02-security.md)
from prose into **tests that fail the build** if the invariant is violated.

Invariant tests:
- **Zero-knowledge cloud:** a test that scans cloud handlers/logs and asserts
  `encrypted_key` is never logged, never passed to any decrypt call, and only ever
  forwarded to the printer link. Capture cloud logs during the Part 5 E2E and assert
  no ciphertext key, no plaintext, no passphrase appears.
- **Plaintext only in RAM/tmpfs:** during the E2E, assert no `*.pdf` plaintext is
  ever written under any bind-mounted volume; `/dev/shm` empty post-delivery; the
  decrypt path never touches a disk path outside `/dev/shm`.
- **mTLS on every hop (negative tests):** a client without a valid client cert is
  **rejected** on the internal printer-link WebSocket and any internal endpoint;
  a wrong-CA cert is rejected. A passing connection alone is not enough — the
  *refusal* is the security property.
- **Key zeroization:** assert passphrase env is cleared after key load and sensitive
  slices are zeroed (already partly covered — make it an explicit guard).

Scanning (SAST / supply chain / secrets):
- `govulncheck ./...` (Go stdlib + dep CVEs) and `gosec` (SAST) in CI.
- `npm audit` / `osv-scanner` for the portal.
- Secret scanner (`gitleaks`) in the pre-commit hook and CI — enforces the
  `.env`/`*.pem`/`certs/` gitignore promise.
- A dependency pin/lockfile check so builds are reproducible.

**Why it matters (interview):** "my CI fails if someone logs the encrypted key, if a
plaintext PDF hits any disk, or if the printer link accepts a certless client" is the
strongest possible statement that the security model is *enforced*, not aspirational.
This is the Part that makes the whole project defensible.

**Verify:** each invariant test fails when you deliberately violate it (log the key,
write a plaintext file, drop the client-cert requirement) and passes otherwise;
`govulncheck`, `gosec`, `gitleaks` run clean in CI.

---

## Part 7 — Resilience & chaos (local)

**Goal:** prove graceful behavior when dependencies and nodes fail — the scenarios a
distributed system faces in production.

Tasks:
- **Printer reconnect:** kill the printer container mid-session; confirm exponential
  backoff + jitter reconnect (unit-tested already — now prove it end to end) and that
  in-flight jobs re-queue rather than vanish.
- **Owner-node failover:** with two cloud nodes, kill the node holding the printer
  socket; confirm `XAUTOCLAIM` reclaims un-ACKed jobs and dispatch resumes on the
  surviving node.
- **Dependency restarts:** bounce Redis and Postgres; confirm the cloud server
  reconnects, no job is double-printed (idempotency / `SELECT FOR UPDATE NOWAIT`
  holds), and no job is silently dropped.
- **Backpressure:** printer offline while N jobs submit → all land in `jobs:pending`
  Stream; printer returns → all drain exactly once (no dup, no loss).

**Why it matters (interview):** "what happens if the node holding the socket dies"
is a classic senior distributed-systems question. You'll have run it, not guessed.

**Verify:** a scripted `make chaos` scenario kills each component in turn; every job
reaches a terminal state exactly once; logs show reconnect, not crash.

---

## Part 8 — Performance & load (k6)

**Goal:** know the system's limits and that it degrades gracefully, not catastrophically.

Tasks:
- **k6** scripts (run locally against compose):
  - Job submission throughput: ramp concurrent guest submissions; watch p95 latency
    and error rate; find the knee.
  - **SSE fan-out:** many concurrent `/jobs/:id/stream` subscribers on one job —
    confirm memory/goroutine count stays bounded (this is the most likely scaling
    surprise).
  - Dispatch throughput: sustained job rate through the Redis Stream; confirm the
    consumer group keeps up and lag is bounded.
- Capture a **baseline** (numbers committed to the repo) so future changes can be
  compared — regression detection, not vanity numbers.
- Run under `-race`-free release builds but watch goroutine/heap via pprof during load.

**Why it matters (interview):** senior candidates quantify. "Submission holds p95 <
X ms to N concurrent; SSE fan-out is bounded because subscribers share one Redis
subscription per job" beats "it seems fast."

**Verify:** `make load` produces a report; a committed baseline exists; a deliberate
N+1 or unbounded-goroutine regression is visible against the baseline.

---

## Part 9 — Pre-production gates (observability + release checklist)

**Goal:** the operational scaffolding that makes a system *supportable*, plus a
go/no-go checklist.

Tasks:
- **Observability under test:** assert structured logs carry correlation IDs
  (job_id / mailbox_id) end to end; add/verify health and readiness endpoints;
  confirm no secret ever appears in a log line (ties to Part 6).
- **Runbook** (`docs/runbook.md`): how to diagnose a stuck job, a disconnected
  printer, a full Stream — each backed by a Part 7 scenario.
- **Release checklist** (`docs/release-checklist.md`): all Parts green; coverage at
  or above floor; `govulncheck`/`gosec`/`gitleaks` clean; E2E + chaos pass; load
  baseline within tolerance; secrets rotated out of any fixture; `DEV_MODE=false`
  path smoke-tested (as far as possible without the physical CUPS printer — the
  Phase 10 owner-blocked boundary).
- **Study doc:** add `docs/study/17-testing-strategy.md` explaining the pyramid,
  why fakes vs real dependencies, and how the security invariants are enforced —
  per the `docs/study/` deliverable convention.

**Verify:** the release checklist can be walked top to bottom and every item maps to
a command or test that produces a green result (except the physical-print step, which
stays documented-as-blocked).

---

## Suggested execution order & effort

| Part | Payoff | Rough effort |
|---|---|---|
| 0 — CI/gates | Everything else stops regressing | S |
| 3 — Crypto contract | Closes the single biggest real risk | M |
| 6 — Security invariants + scanning | Makes the design defensible | M |
| 1 — Unit + fuzz + race | Cheap, high-frequency safety net | S–M |
| 2 — Integration (real deps) | Promotes fakes → real where it counts | M |
| 4 — Portal (Vitest + Playwright) | Covers the untested half | M–L |
| 5 — Full-system E2E | Proves the assembled product | M |
| 7 — Resilience & chaos | Answers the distributed-systems questions | M |
| 8 — Load (k6) | Quantifies limits | S–M |
| 9 — Pre-prod gates | Operable + a clean go/no-go | S |

Recommended first sprint: **0 → 3 → 6**. That trio gives you a defended security
model, a protected crypto seam, and a pipeline that keeps both from regressing —
the highest-signal starting point for both production readiness and interviews.
