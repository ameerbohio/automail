# Kubernetes & distributed systems — at a glance

*Full method, instruments and caveats: [infra/k8s/RESULTS.md](../infra/k8s/RESULTS.md). Deep version: [docs/study/28-kubernetes-orchestration.md](../docs/study/28-kubernetes-orchestration.md).*

The stateless tier of an end-to-end-encrypted mail platform, running on a 4-node Kubernetes
cluster. The orchestration is the visible part; the harder part is that **multiple replicas must
never print the same letter twice**.

Every row and bullet below carries a **see** pointer: the code that drives the scenario, and the
recorded run that measured it.

## What was tested, and what happened

| Scenario | Result | See |
|---|---|---|
| **Rolling update** while traffic keeps flowing | All 3 API pods replaced in **21 s**, **0 failed requests out of 996** | driver: [`scenarioRollingUpdate`](../tests/system/k8s_failure_test.go#L444) · run: [RESULTS.md — Goal K6](../infra/k8s/RESULTS.md#L23) · config: [`maxUnavailable: 0`](../infra/k8s/base/cloud-server/deployment.yaml#L31) |
| **Autoscaling** under load (k6) | **2 → 7 pods** automatically; p95 latency **10.79 ms → 5.80 ms**, 0 % errors | driver: [`scripts/k8s/load-check.sh`](../scripts/k8s/load-check.sh) · run: [RESULTS.md — Goal K7](../infra/k8s/RESULTS.md#L110) · config: [`hpa.yaml` (min 2 / max 8, 60 % CPU)](../infra/k8s/base/cloud-server/hpa.yaml#L21) |
| **Scaling back down** after load stops | Automatic, **283 s** later — deliberately damped so a traffic dip can't cause an outage | the damping itself: [`stabilizationWindowSeconds: 300`](../infra/k8s/base/cloud-server/hpa.yaml#L72) · run: [RESULTS.md — Goal K7](../infra/k8s/RESULTS.md#L110) |
| **Killing the pod** holding the printer's live connection | Printer reconnected to a **different** pod in ~1 s; queued jobs still delivered, **none lost, none printed twice** | driver: [`scenarioSocketOwnerDeleted`](../tests/system/k8s_failure_test.go#L142) · the redial loop: [`wsclient.go`](../services/printer/wsclient.go) · run: [RESULTS.md — Goal K6](../infra/k8s/RESULTS.md#L23) |
| **Node drain / disruption budget** | Kubernetes **refused** the eviction that would have breached availability; the drain itself finished in **8 s** | driver: [`scenarioDrainAgainstPDB`](../tests/system/k8s_failure_test.go#L350) · the budget: [`minAvailable: 2`](../infra/k8s/base/cloud-server/pdb.yaml#L25) · run: [RESULTS.md — Goal K6](../infra/k8s/RESULTS.md#L23) |
| **Crash mid-job** | A job orphaned by a dead node was recovered by another node and delivered **exactly once** | driver: [`scenarioPELHolderDeleted`](../tests/system/k8s_failure_test.go#L266) · the reclaim: [`Dispatcher.reclaim` (XAUTOCLAIM)](../services/cloud/dispatch/dispatcher.go#L215) · run: [RESULTS.md — Goal K6](../infra/k8s/RESULTS.md#L23) |

The instruments used to force each failure are declared rather than hidden.
**see** [RESULTS.md — "Three instruments, declared"](../infra/k8s/RESULTS.md#L39) ·
[RESULTS.md — "Four instruments, declared"](../infra/k8s/RESULTS.md#L126)

## The correctness underneath it

Autoscaling a tier that isn't safe to replicate just multiplies the bug. Three mechanisms make the
replica count free to change, and all three predate the cluster:

- **Exactly-once dispatch across nodes** — a Redis Streams consumer group, so a job is delivered to
  one node; if that node dies mid-job the entry stays in its pending list and another node reclaims
  it only once it is *provably* abandoned.
  **see** [`Dispatcher.drain` (XREADGROUP)](../services/cloud/dispatch/dispatcher.go#L125) ·
  [`Dispatcher.reclaim` (XAUTOCLAIM, with the min-idle window)](../services/cloud/dispatch/dispatcher.go#L215) ·
  [`RemoveConsumer`, and why a consumer holding a PEL is skipped](../services/cloud/dispatch/dispatcher.go#L244) ·
  proved against real Redis: [`TestIntegration_XAutoClaimReclaims`](../services/cloud/integration_redis_test.go#L101) ·
  [the explainer](../docs/study/14-redis-streams-consumer-groups.md)
- **A database-level guard** — `SELECT FOR UPDATE NOWAIT` is the authoritative double-dispatch
  block, so correctness never depends on the queue alone.
  **see** [the query](../services/cloud/db/queries.sql#L103) ·
  [the claim transaction that uses it](../services/cloud/dispatch/route.go#L350) ·
  proved under real contention: [`TestIntegration_SelectForUpdateNowaitContention`](../services/cloud/integration_postgres_test.go#L115) ·
  [the explainer](../docs/study/15-select-for-update-nowait.md)
- **Draining rather than dying** — on shutdown a node finishes in-flight requests, closes live
  status streams deliberately, and deregisters its queue consumer instead of leaking it.
  **see** [the ordered shutdown sequence](../services/cloud/main.go#L382) ·
  [`runShutdown` and its single deadline](../services/cloud/shutdown.go#L30) ·
  [`Hub.Close` — the hijacked printer sockets get `StatusGoingAway`, not EOF](../services/cloud/link/hub.go#L115) ·
  [the acceptance test](../tests/system/shutdown_test.go) · [`make shutdown-check`](../Makefile#L104) ·
  [the explainer](../docs/study/24-graceful-shutdown-consumer-lifecycle.md)

## Stack

| Piece | See |
|---|---|
| Kustomize base + overlays | [`infra/k8s/base/`](../infra/k8s/base/) · [`infra/k8s/overlays/`](../infra/k8s/overlays/) · [validated in CI with no cluster](../scripts/k8s/validate.sh) |
| HPA · PodDisruptionBudget | [`hpa.yaml`](../infra/k8s/base/cloud-server/hpa.yaml) · [`pdb.yaml`](../infra/k8s/base/cloud-server/pdb.yaml) |
| Traefik ingress (IngressRoute, middlewares, sniStrict) | [`infra/k8s/base/ingress/`](../infra/k8s/base/ingress/) |
| StatefulSets for the data tier | [`infra/k8s/base/data/`](../infra/k8s/base/data/) |
| Downward API · readiness/liveness split | [`deployment.yaml` — `NODE_ID` from `fieldRef`](../infra/k8s/base/cloud-server/deployment.yaml#L94) · [probes](../infra/k8s/base/cloud-server/deployment.yaml#L191) |
| The printer stays *outside* the cluster, dialing in over mTLS | [`k8s_test.go`](../tests/system/k8s_test.go) · [the explainer](../docs/study/27-printer-dial-in-outside-the-cluster.md) |
| Go · PostgreSQL · Redis Streams · MinIO (S3) · k6 | [`services/cloud/`](../services/cloud/) · [`scripts/load/`](../scripts/load/) |
| Design doc | [`plans/16-kubernetes.md`](../plans/16-kubernetes.md) |

## How the numbers were produced

Every figure above was **written into RESULTS.md by the scripted run that produced it** — not typed
in afterwards. Each run is a pass/fail gate, so a regression breaks the build rather than quietly
changing a number. The autoscaling run measures against its own single-replica control on the same
cluster, minutes apart, rather than against a number imported from a different topology.

**see** [the writer that splices measurements between BEGIN/END markers](../tests/system/k8s_failure_test.go#L605) ·
[`make k8s-failure`](../Makefile#L203) · [`make k8s-load`](../Makefile#L207) ·
[the control-vs-autoscaled method](../infra/k8s/RESULTS.md#L115)

## What these numbers are not

| Limit | See |
|---|---|
| A **4-node local cluster on one developer machine** — not a production SLA | [`scripts/k8s/up.sh`](../scripts/k8s/up.sh) — 1 server + 3 agents, created by k3d · [the pinned versions](../scripts/k8s/versions.env) |
| The database tier is deliberately single-node | [the StatefulSets, each `replicas: 1`](../infra/k8s/base/data/) |
| The single replica in the load test was never saturated, so the result shows proportional response, not rescue from collapse | [RESULTS.md — "What this does *not* prove"](../infra/k8s/RESULTS.md#L196) |
| One acceptance clause is unmet and stays open: SSE does not survive the reverse-proxy edge | [the open question](../docs/study/00-interview-pending-questions.md) · [the `test.fixme` that names it every run](../services/portal/tests/ingress/ingress.spec.ts) |
