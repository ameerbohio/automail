# Testing & quality engineering — at a glance

*The written strategy: [docs/testing-plan.md](../docs/testing-plan.md) — 10 parts, each with an acceptance line defining "done". Deep version: [docs/study/30-testing-strategy.md](../docs/study/30-testing-strategy.md).*

Most personal projects have unit tests. This one was hardened deliberately, in a planned sequence,
against the failure modes that actually bite in production.

Every row and bullet below carries a **see** pointer to the file that proves it.

## The layers, and what each one exists to catch

| Layer | What it does | Catches | See |
|---|---|---|---|
| **Unit + race** | 127 Go test/fuzz functions, all run under `-race` | Logic errors; data races that only appear under concurrency | [`services/cloud/`](../services/cloud/) and [`services/printer/`](../services/printer/) `*_test.go` (112) + [`tests/system/`](../tests/system/) (15) · [`make test-race`](../Makefile#L47) · [the `go` CI job](../.github/workflows/ci.yml#L11) |
| **Fuzzing** | Go native fuzzing on both wire-protocol frame parsers and on document decryption, seeded from real interop vectors | Panics and crashers on malformed or hostile input | [`FuzzFrameUnmarshal` (cloud)](../services/cloud/link/frames_fuzz_test.go#L15) · [`FuzzFrameUnmarshal` (printer)](../services/printer/frames_fuzz_test.go#L14) · [`FuzzDecryptDocument`](../services/printer/crypto_fuzz_test.go#L32) · [`make fuzz`](../Makefile#L59) |
| **Contract** | Browser TypeScript encrypts → Go decrypts, byte-for-byte, in CI | Two languages that each pass their own tests but disagree with each other | [`crypto-contract.contract.test.ts`](../services/portal/lib/crypto-contract.contract.test.ts) · [`crypto_contract_test.go`](../services/printer/crypto_contract_test.go#L71) · [the vectors](../testdata/crypto-contract/) |
| **Integration** | Real Postgres, Redis and MinIO in containers — not mocks | Behaviour only the real dependency has | `SELECT FOR UPDATE NOWAIT` under contention: [`TestIntegration_SelectForUpdateNowaitContention`](../services/cloud/integration_postgres_test.go#L115) · Streams reclaim: [`TestIntegration_XAutoClaimReclaims`](../services/cloud/integration_redis_test.go#L101) · pre-signed round-trips: [`TestIntegration_PresignedPutGetRoundTrip`](../services/cloud/integration_minio_test.go#L60) · audit immutability: [`TestIntegration_AuditTriggerBlocksMutation`](../services/cloud/integration_postgres_test.go#L60) |
| **Security invariants** | Build-failing guards, each with a sibling test that violates the invariant on purpose | The guard silently not working | [`services/cloud/security_invariants_test.go`](../services/cloud/security_invariants_test.go) · [`services/printer/security_invariants_test.go`](../services/printer/security_invariants_test.go) · [`blob_readpath_test.go`](../services/cloud/blob_readpath_test.go) · [the full table of what each one enforces](security.md) |
| **Browser E2E** | Playwright over the real stack: guest, account and admin journeys | Whole-flow breakage; it caught two CSP bugs `curl` structurally cannot see | [`tests/browser/`](../services/portal/tests/browser/) — [guest](../services/portal/tests/browser/guest.spec.ts), [account](../services/portal/tests/browser/account.spec.ts), [admin](../services/portal/tests/browser/admin.spec.ts) · [`make test-e2e`](../Makefile#L92) · the CSP those tests fixed: [`middlewares.yaml`](../infra/k8s/base/ingress/middlewares.yaml#L32) |
| **Full-system E2E** | One encrypted job, portal → cloud → dispatch → printer → status, plus the two-node case | The assembled product, including cross-node routing | [`tests/system/fullstack_test.go`](../tests/system/fullstack_test.go) · [`make test-e2e-full`](../Makefile#L98) · [what each system suite proves](../tests/system/README.md) |
| **Chaos** | Kill the printer mid-session, kill the owning node, bounce Redis/Postgres, flood a backlog | Recovery paths that are only ever exercised by failure | [`tests/system/chaos_test.go`](../tests/system/chaos_test.go) — the four scenarios: [`redis_bounce`](../tests/system/chaos_test.go#L59), [`postgres_bounce`](../tests/system/chaos_test.go#L71), [`owner_node_failover`](../tests/system/chaos_test.go#L87), [`printer_crash_backpressure`](../tests/system/chaos_test.go#L128) · [`make chaos`](../Makefile#L110) |
| **Load** | k6 with a committed baseline; a regression fails the build | Latency creep, goroutine leaks, unbounded queue growth | [`submission.js`](../scripts/load/submission.js), [`sse_fanout.js`](../scripts/load/sse_fanout.js), [`dispatch_burst.js`](../scripts/load/dispatch_burst.js) · [the committed baseline](../scripts/load/baseline.json) · [the detector that fails the build](../scripts/load/check-baseline.py) · [`make load`](../Makefile#L134) |

## Two habits worth more than the layer count

**Coverage floors that ratchet.** Per-module minimums that may be raised and never lowered, checked
in CI. Note the floors are modest and deliberately so: they cover tested packages only, and the real
assurance here comes from the techniques above, not from a percentage. A high coverage number bought
by asserting nothing is worse than a low one next to a fuzz corpus.
**see** [`scripts/coverage.floors`](../scripts/coverage.floors) (the numbers) ·
[`scripts/coverage.sh`](../scripts/coverage.sh) (the gate) ·
[the `coverage` CI job](../.github/workflows/ci.yml#L34)

**The failure modes are documented as runbook entries.** Every scenario — stuck job, disconnected
printer, backing-up queue — is backed by a chaos scenario that actually produces it. The runbook is
not aspirational prose.
**see** [`docs/runbook.md`](../docs/runbook.md) alongside
[the chaos scenarios that reproduce each one](../tests/system/chaos_test.go#L59)

## CI

Eight parallel jobs on every push. Locally it is one command: `make ci`.
**see** [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) · [`make ci`](../Makefile#L219)

| Job | See |
|---|---|
| Go build/vet/gofmt/race, per module | [`go`](../.github/workflows/ci.yml#L11) |
| Coverage floor | [`coverage`](../.github/workflows/ci.yml#L34) |
| Fuzz budget | [`fuzz`](../.github/workflows/ci.yml#L45) |
| Portal typecheck + unit coverage | [`portal`](../.github/workflows/ci.yml#L57) |
| Cross-language crypto contract | [`crypto-contract`](../.github/workflows/ci.yml#L81) |
| Security scanners | [`security`](../.github/workflows/ci.yml#L101) |
| Kubernetes manifest validation (no cluster needed) | [`manifests`](../.github/workflows/ci.yml#L127) |
| Integration suite against real dependencies | [`integration`](../.github/workflows/ci.yml#L145) |

Release readiness is itself a checklist where **every item maps to a green command**, with
environment-gated items deferred with a stated reason rather than skipped.
**see** [`docs/release-checklist.md`](../docs/release-checklist.md) ·
[`make help`](../Makefile#L17) (43 documented targets)

## What this is not

| Limit | See |
|---|---|
| Coverage percentages are not the headline and should not be read as one | [the floors, and why they are modest](../scripts/coverage.floors) |
| No mutation testing, no property-based testing beyond fuzzing | [the fuzz targets, which are the whole of it](../scripts/fuzz.sh) |
| Load numbers come from a single developer machine — regression tripwires against *this* host, not throughput claims | [the baseline, labelled as such](../scripts/load/baseline.json) · [the "what this does not prove" section](../infra/k8s/RESULTS.md#L196) |
| Risks deliberately accepted rather than fixed, with re-review triggers | [`docs/accepted-risks.md`](../docs/accepted-risks.md) |
