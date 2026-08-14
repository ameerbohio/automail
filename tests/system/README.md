# System tests

The suites that drive the **assembled product** from outside — over its real
HTTP contract and the browser's exact crypto wire format — rather than any one
service's internals. Per-service unit and integration tests live next to their
code (`services/*/`); the portal's browser tests live in
`services/portal/tests/`.

This is a standalone Go module (`automail/tests/system`) with **zero external
dependencies**: everything is standard library, so the security scanners have
no new supply chain to audit and the cloud/printer modules stay untouched.

## The suites

Every suite is behind a build tag, so none of them run under a default
`go test ./...`. Each needs a live stack, which its script brings up.

| Suite | Tag | Run it with | Brought up by | Proves |
|---|---|---|---|---|
| [fullstack_test.go](fullstack_test.go) | `e2e` | `make test-e2e-full` | [scripts/e2e/full.sh](../../scripts/e2e/full.sh) | One encrypted job through the whole two-node Compose stack to `delivered`, plus the `/dev/shm` wipe (Goal T8) |
| [chaos_test.go](chaos_test.go) | `chaos` | `make chaos` | [scripts/e2e/chaos.sh](../../scripts/e2e/chaos.sh) | Kill Redis, Postgres, the socket-owning node and the printer in turn — every job still reaches a terminal state exactly once (Goal T9) |
| [deploy_smoke_test.go](deploy_smoke_test.go) | `smoke` | `make deploy-smoke` | [scripts/deploy/smoke.sh](../../scripts/deploy/smoke.sh) | The **production profile** — base compose only, reached solely through the Traefik HTTPS edge on routed hostnames (Goal T12) |
| [shutdown_test.go](shutdown_test.go) | `shutdown` | `make shutdown-check` | [scripts/shutdown/check.sh](../../scripts/shutdown/check.sh) | A draining server *deliberately* ends an open SSE stream instead of severing it (Goal K0) |
| [k8s_test.go](k8s_test.go) | `k8s` | `make k8s-e2e` | [scripts/k8s/e2e.sh](../../scripts/k8s/e2e.sh) | A job through the k3d ingress to a printer that is **not** a workload, plus Redis fan-in across pods (Goal K5) |
| [k8s_failure_test.go](k8s_failure_test.go) | `k8sfail` | `make k8s-failure` | [scripts/k8s/failure-check.sh](../../scripts/k8s/failure-check.sh) | Pod kill, PDB eviction/drain and a rolling update, all under live traffic (Goal K6) |

## Shared, non-test files

| File | Tags it builds under | What it holds |
|---|---|---|
| [harness.go](harness.go) | all six | The deployment-agnostic core: the product's HTTP contract, browser-identical encryption, SSE reading, compose helpers |
| [edge.go](edge.go) | `smoke`, `k8s`, `k8sfail` | HTTPS-edge plumbing — SNI and the self-signed edge certificate, so `https://api.automail.local/…` resolves to wherever the edge actually is |
| [k8s_cluster.go](k8s_cluster.go) | `k8sfail` | Cluster manipulation and observation: kubectl, redis-cli, psql, docker. Deliberately separate from `harness.go`, which knows nothing about Kubernetes |

## Why one package rather than one per suite

The six suites share ~55 helpers through `harness.go`. Splitting them into
separate packages would mean exporting that entire surface for no behavioural
gain — the build tags already give complete isolation, and `go vet -tags <tag>`
type-checks each suite independently.
