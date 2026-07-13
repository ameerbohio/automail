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

## What's proven locally vs. what a real deployment adds

All of the above runs on one laptop. What it deliberately *doesn't* reproduce:
production traffic, canary rollouts, error budgets, and on-call feedback into the
suite. Local chaos tests *simulate* failure; they don't replace having survived it.
Naming that boundary honestly is part of the strategy.
