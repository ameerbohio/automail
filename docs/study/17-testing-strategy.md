# Testing Strategy

How Automail is tested to a production bar, and the reasoning behind each layer.
The executable spec is [../testing-plan.md](../testing-plan.md); this is the
interview-oriented "why."

## The pyramid, and why this shape

Many fast unit tests, fewer integration tests, a thin layer of slow end-to-end —
the standard shape (Google's Small/Medium/Large taxonomy). An inverted pyramid
(mostly E2E) is slow and flaky; skipping the middle ("ice-cream cone") means
integration bugs only surface in E2E where they're expensive to localize.

```
        E2E / chaos / load        (few, slow, real processes over mTLS)
        integration (real PG/Redis/MinIO via testcontainers)
        unit + fuzz + race        (many, fast, run on every save)
   cross-cutting: CI gates, security-invariant guards, scanners
```

## Fakes vs. real dependencies — a deliberate axis

The unit/handler tests run against a **fake `database/sql` driver**
(`dbfake_test.go`) and **miniredis**, not real servers: fast, hermetic, and they
still exercise the real sqlc query layer and Redis command paths. That proves our
code calls the right methods.

What a fake *cannot* prove is that Postgres actually honors `SELECT FOR UPDATE
NOWAIT`, that a Redis Streams consumer group survives a crash, or that a pre-signed
MinIO URL round-trips bytes the cloud never touches. Those behaviors are promoted to
**real dependencies via `testcontainers-go`** (Part 2, `integration_*_test.go`,
build tag `integration`, run by `make test-integration`). Each suite spins its own
ephemeral container and tears it down via `t.Cleanup`, so the tests stay hermetic
despite using real servers. The exact behaviors moved fake → real:

- **Postgres** — `schema.sql` applies clean and pgcrypto round-trips; the
  `audit_immutable` trigger genuinely rejects `DELETE`/`UPDATE` on `audit_events`
  (prose promise → executable guard); `LockJobForDispatch` returns `55P03`
  *immediately* under contention instead of blocking (the whole point of `NOWAIT`),
  and a terminal job is not claimable.
- **Redis** — the `XADD → XREADGROUP(">") → XACK` cycle empties the PEL; an
  un-ACKed entry is reclaimed by `XAUTOCLAIM` under a *different* consumer (the
  node-crash recovery path); pub/sub and pattern (`PSUBSCRIBE`) delivery cross a
  *separate connection* — the fan-out that lets a job claimed on a non-owner node
  still reach the owner.
- **MinIO** — pre-signed PUT then GET round-trips ciphertext with the cloud never in
  the byte path (a companion **static AST guard**, `blob_readpath_test.go`, fails the
  build if any cloud code calls `GetObject`/`PutObject`); `RemoveBlob` deletes.

Being able to name exactly which behaviors you moved fake → real, and why, is the
senior signal — not "I used mocks" or "I used real everything."

The teardown case earns its own assertion: killing a container mid-test must produce
a **prompt, explained error, not a hang** (`TestIntegration_TornDownContainerFailsCleanly`
bounds the call with a context and an outer timer). A dependency that dies should
surface fast, not wedge a goroutine.

## Adversarial input: fuzzing

Every byte-parser reachable from the network is fuzzed with Go's native fuzzer
(Part 1): the printer-link frame parsers and `DecryptDocument`. A zero-knowledge
system must assume the input is hostile — so we assert the decrypt path never
panics and never returns bytes alongside an error, and the frame parsers never
panic on a malformed frame. Fuzzing the frame boundary is justified because it's
directly reachable over the mTLS WebSocket hop.

## Security invariants as executable guards

The non-negotiables in [CLAUDE.md](../../CLAUDE.md) / `plans/02-security.md` are
enforced by tests that **fail the build** (Part 6), not by prose:

- **Zero-knowledge cloud** — an AST scan asserts no cloud code logs an
  `encrypted_key` value and nothing calls a `Decrypt*` routine.
- **Plaintext only in tmpfs** — `tmpfsDir` is under `/dev/shm`, and an AST scan
  (with light dataflow) asserts every file write is tmpfs-derived.
- **mTLS on every hop** — a negative test drives the real `internalTLSConfig` and
  asserts a certless / wrong-CA client is *refused*. The refusal is the property;
  a passing connection alone proves nothing.
- **Passphrase hygiene** — `loadDocKey` unsets `PRINTER_KEY_PASSPHRASE` from the
  environment even when key loading fails.

The interview line: "my CI fails if someone logs the encrypted key, writes
plaintext to disk, or lets a certless client onto the printer link" — the security
model is *enforced*, not aspirational.

## The gates that keep it honest

- **Ratcheting coverage floors** (per module, may rise never fall) — resist the
  gaming a fixed target invites.
- **Race detector** on every run — the WebSocket-fan/pub-sub/SSE goroutines are
  where data races live.
- **Scanners** — `govulncheck` (patched via the pinned toolchain), `gosec`
  (genuine findings fixed, intentional cases annotated), `gitleaks` (secrets).
  `npm audit` is informational; accepted findings live in
  [../accepted-risks.md](../accepted-risks.md).

## Browser E2E: proving the zero-knowledge claim from the client side

The top of the pyramid (Part 4b / `make test-e2e`) drives a real Chromium
against the whole assembled stack over `docker-compose` — portal → cloud →
Redis dispatch → printer decrypt → SSE status — for the three journeys a user
actually takes: guest submit-and-track, account history, and the admin
dashboard. Two ideas make it worth more than the sum of its unit tests:

- **The ciphertext-on-wire assertion.** The guest test embeds a unique plaintext
  marker in the PDF, then intercepts the browser's `PUT` to object storage and
  asserts the bytes are *not* the plaintext (no marker, no `%PDF` magic, not
  equal to the input). That is the project's whole thesis — "the server only
  ever stores ciphertext" — demonstrated from the client, which is more
  convincing than any diagram. It works because encryption happens in the tab
  (Web Crypto) before the direct-to-storage upload the cloud never touches.
- **Split object-storage endpoint.** The browser can't resolve the internal
  `minio:9000` service name, and a SigV4 URL binds the host it's signed for, so
  the upload URL must be signed for a host the browser reaches. The cloud signs
  it with a dedicated public-endpoint client (`MINIO_PUBLIC_ENDPOINT`) while all
  *server-side* blob ops keep the internal client — the real internal-vs-public
  S3 endpoint pattern, not a test hack.

**Two real bugs this layer caught** (the payoff of testing the assembled product,
not just the parts):

1. **Idle-printer liveness.** The keepalive sent WebSocket *pings*, but the
   cloud's dispatch-eligibility cache (`mailbox:<id>:state`, 90s TTL) is only
   refreshed by register/`state` frames. A connected-but-idle printer silently
   fell out of the dispatchable set after 90s. Fix: the keepalive now also
   re-sends a `state` frame each tick (plans/04 is literally titled "Keepalive
   *and State Reporting*"). Unit tests never saw it because they used fakes with
   matching state already seeded.
2. **Slot-identity contract.** The dev printer reported occupancy under the
   literal key `"slot-1"`, but eligibility looks the slot up by the DB
   `mailbox_slots.id` (plans/04: `slot_occupancy` is keyed by `<slot_id>`), so
   the lookup never matched and every job queued forever. Fix: the printer's
   slot id is now configuration (`SLOT_ID`, like `MAILBOX_ID`), set to the real
   slot UUID in a deployment.

Both are classic "each service is individually correct, the *seam* between them
isn't" bugs — exactly what an end-to-end test exists to find.

## Full-system E2E: proving the *distributed* seams

The browser E2E above proves the product from a user's seat, but it runs a
single cloud node, so it can't exercise the design's most interview-relevant
claim: that the cloud scales horizontally because Redis — not any one process —
is the dispatch fan-in and status fan-out backbone. Part 5 / `make test-e2e-full`
closes that gap with a standalone Go driver (`e2e/`, zero external deps — it just
speaks the public HTTP contract and the same crypto wire format the browser uses)
against a **two-node** stack (`docker-compose.full.yml`).

The whole test turns on one topological fact: the printer's dial-out mTLS socket
lands on exactly one cloud node. We pin that by naming the two nodes — the
printer only ever dials the `cloud-server` alias, so `cloud-server` is always the
socket **owner** and `cloud-server-2` is always the **non-owner**. (The roadmap
says `--scale cloud-server=2`; scaled replicas share one alias and can't each
publish a host port, so a driver couldn't *deterministically* address the
non-owner. Two named nodes exercise the identical property and make the targeting
deterministic — the reasoning is written up in the compose file header.)

With that pin, two status values become *proofs*, no log-scraping required:

- **Dispatch fan-in.** The driver submits the job to the **non-owner**. A
  `POST /jobs` returning `"dispatching"` is only reachable if that node's
  `PUBLISH mailbox:<id>:dispatch` had a subscriber — and the *only* subscriber is
  the owner node holding the socket. A non-owner with no live socket of its own
  would see `receivers == 0`, revert the claim, and return `"queued"` instead. So
  the single word `dispatching` from the non-owner *is* the cross-node fan-in.
- **Status fan-out.** The driver opens the SSE stream on the **non-owner** too.
  The handler's first event is a DB snapshot, but any *later* event can only have
  arrived over the `job:<id>:status` channel — the printer emitted it on the
  owner's socket, the owner published it, and Redis delivered it to the
  non-owner. Requiring ≥2 events ending in `delivered` (the run saw
  `[printing printing delivered]`) proves the status crossed the node boundary,
  not just a local read.

The same test also promotes `TestHandleDispatch_DeliversAndWipes` from a unit
fake to the real stack: after `delivered`, it execs into the printer container
and asserts `/dev/shm` holds no job file — the RAM-only plaintext invariant,
verified end-to-end on the assembled product rather than in a decrypt unit test.

Why a Go driver here instead of extending the Playwright suite: the two-node
proof needs to address a *specific* node deterministically (submit here, stream
there), which a browser going through one portal origin can't do; and the crypto
the driver needs is byte-identical to the browser's, already proven equivalent by
the Part 3 contract test, so nothing is lost by encrypting in Go.

## Resilience & chaos: killing each moving part

Everything above proves the system works when the parts stay up. `make chaos`
(`scripts/e2e/chaos.sh` → `e2e/chaos_test.go`, build tag `chaos`) proves it
*recovers* when they don't. It reuses the two-node full-system stack and, in one
run, kills each moving part in turn — Redis, Postgres, the socket-owning cloud
node, the printer — asserting two properties after every kill.

**Exactly-once, read from the ledger, not the status.** The tempting assertion is
"the job says delivered." The stronger one is: the append-only `audit_events`
table holds *exactly one* `job_delivered` row for the job. Zero means the job
vanished; two means it was double-printed. That table is the same immutable ledger
the Part 2 integration test proved can't be `UPDATE`d or `DELETE`d, so a count
against it is trustworthy in a way a mutable status column isn't. This is what
turns "no double-print" from a hope into a test — the `SELECT FOR UPDATE NOWAIT`
claim guard and the dispatcher's ACK-only-when-done discipline are what make the
count come out at one.

**Reconnect, not crash.** After each kill a *fresh* job flows to delivered — which
is itself the proof the cloud re-established its Redis/Postgres pools, since those
pools are the only way a job moves. On top of that the printer's dial loop must
log backoff-and-retry (`reconnecting in …`), and no service log may carry a Go
runtime crash marker (`panic:` / `fatal error:`). A logged *connection error*
during a bounce is expected resilience, not a failure — only a crash marker fails
the test.

Two scenarios are worth calling out:

- **Backpressure (printer offline).** Kill the printer, submit N jobs while it's
  down. Each must return `"queued"` and land in the `jobs:pending` stream (with
  the printer gone, `PUBLISH mailbox:<id>:dispatch` has zero receivers, so
  dispatch reverts the claim and enqueues instead of writing to a dead socket).
  Restart the printer; the whole backlog drains, each delivered exactly once,
  `/dev/shm` clean. This is the queue earning its keep — the buffer that turns a
  printer outage into latency instead of lost mail.
- **Owner-node failover.** Stop the cloud node that holds the printer socket. The
  survivor node keeps taking submissions; with no live socket anywhere they
  enqueue rather than vanish (verified against `jobs:pending` depth). Bring the
  owner back, the printer re-homes on it, and the backlog drains exactly once. An
  honest boundary the test documents: because the printer only ever *dials* the
  `cloud-server` alias, the socket can't fail *over* to the survivor in this
  pinned topology — the survivor's role is to keep accepting and buffering work,
  and the crashed node's un-ACKed stream entries are recovered by `XAUTOCLAIM`
  (exercised directly in the Part 2 Redis integration test, where a second
  consumer reclaims a dead consumer's PEL entry).

One design constraint the suite has to respect: the dev printer reports a single
slot whose occupancy only ever *increments* in-process and resets only when the
printer process restarts. So the scenarios are ordered and budgeted — the three
that leave the printer running stay under its slot cap, and the printer-restart
scenario (which resets it) runs last. That's a note-to-self about *test* fixtures,
not the product; a real mailbox's slot empties when mail is collected.

## Load & performance: quantifying, and detecting regressions

`make load` (Part 8) is the layer that replaces "it seems fast" with numbers, and
— more importantly — with a **committed baseline** so a future change that makes
things worse fails the build. It runs a single-node stack with pprof enabled
(`docker-compose.load.yml`, `PPROF_ADDR` set for that profile only — never in the
base compose or on a deploy host, since pprof exposes heap/goroutine dumps), and
drives k6 from *inside* the compose network, because the cloud signs its presigned
upload URLs for the internal `minio:9000` host.

Three phases, each answering a different scaling question:

- **Phase A — submission throughput.** Ramps the guest arrival rate through the
  real three-call flow (upload-url → PUT ciphertext → `POST /jobs`), timing only
  the submit. The `encrypted_key` is synthetic: the cloud stores it verbatim and
  never decrypts it, so the zero-knowledge design is exactly what makes it safe to
  load-test the submission path with junk key material.
- **Phase B — SSE fan-out boundedness**, the most likely scaling surprise. Holds
  N concurrent `/jobs/:id/stream` subscribers on one job while sampling goroutines
  via pprof. The honest finding: `StreamJob` opens **one Redis subscription per
  connection**, so goroutines grow ~linearly (~4 per subscriber ≈ handler +
  go-redis read-loop + `sub.Channel()` pump). Linear growth is *by design*; the
  bug worth catching is not returning to baseline. So the baseline gates growth
  loosely and **residual tightly** — measured residual was *below* idle, i.e. no
  per-connection leak. (A shared per-job subscription would flatten the curve —
  a real optimisation, deliberately not done yet.)
- **Phase C — dispatch backlog drain.** Phase A never touches the Redis Stream:
  with a live printer, jobs go out via *immediate* dispatch. So Phase C stops the
  printer, queues a burst (0 subscribers on `mailbox:<id>:dispatch` ⇒ the claim is
  reverted and the job is enqueued), then restarts the printer and times the
  consumer group draining the backlog to zero. The run guards against a silent
  no-op: if nothing actually queued, the phase fails rather than reporting a
  trivially-clean drain.

**The baseline is the deliverable, not the numbers.** `check-baseline.py` compares
each run against committed ceilings/floors and exits non-zero on a breach, and
`make load-selftest` proves the detector actually bites by running it against a
deliberately-regressed fixture (latency blowout, throughput collapse, stalled
dispatch, and the classic unbounded-goroutine leak) and asserting it fails.
Calibrating the bounds mattered: the first honest run showed ~3.97 goroutines per
subscriber against a guessed ceiling of 3 — the *bound* was wrong, not the code,
and widening it to 5 (with the reasoning written down) kept it strict enough that
the regressed fixture still trips it.

Two bugs the harness itself surfaced, both worth knowing as shell/ops lessons:
`set -o pipefail` plus `curl … | head -1` makes curl die of SIGPIPE, so the
pipeline reports failure even when the match succeeded and the `|| echo 0`
fallback silently corrupted every goroutine reading; and a container writing into
a bind-mounted host directory needs a matching uid — k6's summary export failed
with "permission denied" yet k6 still **exited 0**, so the real failure only
appeared much later as a missing file. Both are now fixed at the source, and the
run fails fast with a pointed message if the export goes missing again.

## Deployment parity: testing the seams the test stacks remove (T12)

Every suite above buys determinism by *changing the topology*. The browser E2E
publishes the portal on `localhost:3000`, the full-system E2E publishes two cloud
nodes on `:8080`/`:8081`, the load profile runs k6 inside the Docker network — all
of them bypass the Traefik edge, because a self-signed TLS front door and
hostname routing are friction a test does not want. That is a sound trade, and it
has a cost that is easy to miss: **the parts of the system those overrides remove
are never tested by anything.**

That gap is not theoretical. Everything about the edge is production-only, and
the first Proxmox bring-up hit failure after failure in exactly that band — a
missing edge certificate against `sniStrict` (blanket TLS rejection), a Traefik
image whose vendored Docker client could not negotiate an API version with a
modern Engine (blanket 404, because every router is a Docker label). Fixing them
one deploy-attempt at a time is the slow way to find out.

So the deployment smoke inverts the usual override: it runs the **base compose
unchanged** and drives everything through `https://api.automail.local`. The
mechanism is worth stealing — Go's `http.Transport` lets you override
`DialContext` while leaving the URL's hostname intact, which is `curl --resolve`
in library form: SNI, the Host header, the router rule and `sniStrict` all behave
exactly as in production, but the socket lands on whatever port Traefik is
published on. The cert is pinned via `RootCAs` rather than waved through with
`InsecureSkipVerify`, so "the edge serves the certificate we generated, for the
name we asked for" becomes an assertion instead of an assumption. Every shared
harness helper then drives real HTTPS URLs with no idea the edge exists.

Writing it found four more production-only defects in one run, and their shape is
the actual lesson — **none of them fails loudly, and none is in application
code**:

- **The pre-signed upload URL was signed against `minio:9000`**, a Docker-internal
  name. By design the browser PUTs ciphertext straight to object storage, so the
  guest flow died at its first step on any real deploy — and the server logged
  nothing, because *handing out* the URL succeeded.
- **`Content-Security-Policy: default-src 'self'` bricked the portal.** Next.js
  App Router streams its RSC payload in inline `<script>` blocks; under that
  policy all five are blocked. The SSR HTML still renders and still returns
  `200`, so the page *looks* fine — but nothing hydrates, no handler binds, and
  clicking Search fires zero requests. The same policy separately blocked the
  cross-origin upload PUT.
- **The rate limit throttled nobody.** `average: 20, period: 1m` read exactly like
  the plan's "20 requests/min", but Traefik defaults `burst` to **1** — one
  request per three seconds. Worse, the middleware was attached only to
  `api.automail.local`, which *the browser never contacts*: the portal calls
  same-origin `/api/*` and Next proxies it server-side, bypassing Traefik
  entirely. So the spec'd control was both mistuned and mounted on the wrong door.
- **The printer's `SLOT_ID` defaulted to the literal `"slot-1"`** while dispatch
  eligibility looks slots up by their database UUID. Result: no eligible slot
  ever, every job parked in `queued` forever, printer reporting healthy, nothing
  logged as an error.

The through-line: **a config default that is *wrong* is far more dangerous than
one that is *missing***, because missing usually crashes at startup where you will
see it. All four were silent by construction. Two also illustrate why a green test
is not the same as a working system — a Go client executes no scripts and enforces
no CSP, so the first version of the CSP assertion passed happily against a portal
that was completely dead in Chromium. The fix was to assert the *parsed policy*
(does `script-src` permit what Next emits, does `connect-src` name the upload
origin) instead of a status code, and to write down plainly that only a real
browser can catch a genuinely novel CSP break. Knowing what your assertion cannot
see is part of writing it.

The one durable production fix beyond the tests was making the silent case speak:
dispatch now logs which slot ids the printer actually reported when the lookup
misses, turning an unexplained hang into a one-line diagnosis.

The checklist (`docs/deploy-checklist.md`) and the smoke script share a
preflight, so the document cannot rot into aspiration: if a prerequisite is not
really required, the script stops checking it; if it is, `make deploy-smoke` fails
with the exact remediation command. Documentation that is also a test is the only
kind that stays true.

## What's proven locally vs. what a real deployment adds

All of the above runs on one laptop. What it deliberately *doesn't* reproduce:
production traffic, canary rollouts, error budgets, and on-call feedback into the
suite. Local chaos tests *simulate* failure; they don't replace having survived it.
Naming that boundary honestly is part of the strategy.
