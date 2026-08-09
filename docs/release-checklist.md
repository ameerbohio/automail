# Release Checklist (go / no-go)

Walk top to bottom. Every gate maps to a command that must produce a green result.
Items marked **⛔ gated** are blocked in an environment that lacks Docker or the
physical printer — they are the deploy-time equivalents and must be run where those
are available; they are not skipped, they are deferred with a stated reason.

## Correctness & quality gates (Docker-free — run now)

| Gate | Command | Expect |
|---|---|---|
| Both Go modules build & vet | `make vet` (+ `go build ./...` per module) | clean |
| gofmt clean | `make fmt-check` | clean |
| Unit tests under the race detector | `make test-race` | pass |
| Security-invariant guards | included in `go test ./...` (mTLS refusal, no-log, tmpfs, passphrase) | pass |
| Fuzz regression sweep | `make fuzz` (or `-fuzztime=30s` per target) | no crashers |
| Go coverage ≥ ratcheting floor | `make cover` (floors live in `scripts/coverage.floors`) | ✔ |
| Portal typecheck | `npx tsc --noEmit` (in `services/portal`) | clean |
| Portal unit coverage ≥ floor | `make cover-portal` (≥39.4) | ✔ |
| Cross-language crypto contract | `make crypto-contract` | byte-for-byte + tamper reject |
| Readiness endpoint | `TestHealthz_Readiness` (200 healthy / 503 store down) | pass |

The umbrella for the local set: **`make ci`** (fmt-check → lint → test-race → cover
→ cover-portal) plus **`make crypto-contract`** and **`make scan`**.

## Supply-chain / secrets

| Gate | Command | Expect |
|---|---|---|
| Go vuln scan | `govulncheck ./...` (both modules; via `make scan`) | 0 affecting |
| SAST | `gosec` (via `make scan`; documented excludes) | 0 |
| Secret scan | `gitleaks git -c .gitleaks.toml` (via `make scan`) | no leaks |
| npm audit (informational) | `npm audit --omit=dev` | findings triaged in **[accepted-risks.md](accepted-risks.md)** |
| No real secrets committed | `.env`/`*.pem`/`certs/` gitignored; only the NON-PROD `testdata/crypto-contract/fixture.json` test key exists | confirm |
| Accepted risks reviewed | read **[accepted-risks.md](accepted-risks.md)** — AR-1 (residual Next.js advisories) still within its re-review triggers | confirm |

## Deploy-time gates (⛔ require Docker / hardware)

| Gate | Command | Status |
|---|---|---|
| Integration vs real Postgres/Redis/MinIO | `make test-integration` | ⛔ Docker (Goal T5) |
| Full-system E2E (encrypt→print→status, /dev/shm clean) | `make test-e2e-full` | ⛔ Docker (Goal T8) |
| Resilience / chaos (reconnect, failover, exactly-once) | `make chaos` | ⛔ Docker (Goal T9) |
| Load baseline within tolerance | `make load` | ⛔ Docker (Goal T10) |
| Production-profile smoke through the HTTPS edge | `make deploy-smoke` | ⛔ Docker (Goal T12) |
| `DEV_MODE=false` printer starts and reaches a CUPS queue | on the CUPS host: `DEV_MODE=false docker compose up -d printer`, then `docker compose logs printer` | ⛔ hardware (needs a real cupsd; the smoke pins `DEV_MODE=true`) |
| First-deploy host prerequisites walked | **[deploy-checklist.md](deploy-checklist.md)** top-to-bottom on the target host | ⛔ target host |
| Correlation IDs present end-to-end in a live run | capture logs during E2E; assert `job_id`/`mailbox_id` present, no secret | ⛔ Docker (rides on T8) |
| Physical print (paper out, `/dev/shm` empty) | submit a job on the mailbox host | ⛔ hardware — owner-blocked Phase 10 |

## Kubernetes target (optional — not required to ship the Compose stack)

The manifests in [`infra/k8s/`](../infra/k8s/) are an **additional** deploy target
for the stateless tier, not a replacement: every gate above still applies to the
Compose path, which is what ships. These gates only matter if the cluster is
being demoed or deployed. `make k8s-validate` is the exception — it is Docker-free,
already part of `make ci`, and catches a manifest typo on the machine that wrote it.

| Gate | Command | Status |
|---|---|---|
| Manifests build + client dry-run + no `:latest` | `make k8s-validate` | Docker-free (part of `make ci`) |
| Data tier applies, schema present, data survives pod deletion | `make k8s-data-check` | ⛔ cluster (Goal K2) |
| Pod spread, Service fan-out, no consumer leak on rollout | `make k8s-cloud-check` | ⛔ cluster (Goal K3) |
| Ingress: 3 hostnames, sniStrict, CSP/presign ports, rate limit | `make k8s-edge-check` + `make k8s-edge-browser` | ⛔ cluster (Goal K4 — one clause open, see below) |
| Printer outside the cluster reaches `delivered`, fan-in holds | `make k8s-e2e` | ⛔ cluster (Goal K5) |
| Pod kill, PDB eviction, node drain, rolling update under traffic | `make k8s-failure` | ⛔ cluster (Goal K6) |
| HPA scales up/down under k6 load vs a single-replica control | `make k8s-load` | ⛔ cluster (Goal K7) |

**Open on this target:** live SSE status does not reach the browser through *any*
Traefik edge — including Compose's — which is a pre-existing portal gap, not a
Kubernetes regression. It is an owner decision logged in
[docs/study/00-interview-pending-questions.md](study/00-interview-pending-questions.md)
and guarded by a `test.fixme`. Measurements for K6/K7 live in
[infra/k8s/RESULTS.md](../infra/k8s/RESULTS.md).

## Go / no-go

- **Local build shippable:** all Docker-free + supply-chain gates green.
- **Deployable to Proxmox:** additionally the ⛔ Docker gates green in an
  environment with the compose stack (Goals T5, T7–T10, T12).
- **Full production sign-off:** additionally the physical-print gate, once the
  owner configures the CUPS host (Phase 10).
