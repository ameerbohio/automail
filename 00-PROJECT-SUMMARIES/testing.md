# Testing & quality engineering — at a glance

*The written strategy: [docs/testing-plan.md](../docs/testing-plan.md) — 10 parts, each with an acceptance line defining "done". Deep version: [docs/study/17-testing-strategy.md](../docs/study/17-testing-strategy.md).*

Most personal projects have unit tests. This one was hardened deliberately, in a planned sequence,
against the failure modes that actually bite in production.

## The layers, and what each one exists to catch

| Layer | What it does | Catches |
|---|---|---|
| **Unit + race** | 127 Go test/fuzz functions, all run under `-race` | Logic errors; data races that only appear under concurrency |
| **Fuzzing** | Go native fuzzing on both wire-protocol frame parsers and on document decryption, seeded from real interop vectors | Panics and crashers on malformed or hostile input |
| **Contract** | Browser TypeScript encrypts → Go decrypts, byte-for-byte, in CI | Two languages that each pass their own tests but disagree with each other |
| **Integration** | Real Postgres, Redis and MinIO in containers — not mocks | Behaviour only the real dependency has: `SELECT FOR UPDATE NOWAIT` under contention, Streams reclaim, pre-signed round-trips |
| **Security invariants** | Build-failing guards, each with a sibling test that violates the invariant on purpose | The guard silently not working |
| **Browser E2E** | Playwright over the real stack: guest, account and admin journeys | Whole-flow breakage; it caught two CSP bugs `curl` structurally cannot see |
| **Full-system E2E** | One encrypted job, portal → cloud → dispatch → printer → status, plus the two-node case | The assembled product, including cross-node routing |
| **Chaos** | Kill the printer mid-session, kill the owning node, bounce Redis/Postgres, flood a backlog | Recovery paths that are only ever exercised by failure |
| **Load** | k6 with a committed baseline; a regression fails the build | Latency creep, goroutine leaks, unbounded queue growth |

## Two habits worth more than the layer count

**Coverage floors that ratchet.** [`scripts/coverage.floors`](../scripts/coverage.floors) holds
per-module minimums that may be raised and never lowered, checked in CI. Note the floors are
modest and deliberately so: they cover tested packages only, and the real assurance here comes from
the techniques above, not from a percentage. A high coverage number bought by asserting nothing is
worse than a low one next to a fuzz corpus.

**The failure modes are documented as runbook entries.** Every scenario in
[docs/runbook.md](../docs/runbook.md) — stuck job, disconnected printer, backing-up queue — is backed by
a chaos scenario that actually produces it. The runbook is not aspirational prose.

## CI

Eight parallel jobs on every push: Go build/vet/gofmt/race per module, coverage floor, fuzz
budget, portal typecheck + unit coverage, the cross-language crypto contract, the security
scanners, Kubernetes manifest validation (no cluster needed), and the integration suite against
real dependencies. Locally it is one command: `make ci`.

Release readiness is itself a checklist where **every item maps to a green command**, with
environment-gated items deferred with a stated reason rather than skipped —
[docs/release-checklist.md](../docs/release-checklist.md).

## What this is not

Coverage percentages are not the headline and should not be read as one. There is no mutation
testing, no property-based testing beyond fuzzing, and the load numbers come from a single
developer machine — they are regression tripwires against *this* host, not throughput claims.

## Where to look

[`docs/testing-plan.md`](../docs/testing-plan.md) ·
[`Makefile`](../Makefile) (23 documented targets — `make help`) ·
[`.github/workflows/ci.yml`](../.github/workflows/ci.yml) ·
[`services/cloud/integration_*_test.go`](../services/cloud/) ·
[`services/portal/e2e/`](../services/portal/e2e/) ·
[`scripts/load/baseline.json`](../scripts/load/baseline.json)
