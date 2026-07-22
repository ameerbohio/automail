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

## What's proven locally vs. what a real deployment adds

All of the above runs on one laptop. What it deliberately *doesn't* reproduce:
production traffic, canary rollouts, error budgets, and on-call feedback into the
suite. Local chaos tests *simulate* failure; they don't replace having survived it.
Naming that boundary honestly is part of the strategy.
