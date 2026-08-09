# Kubernetes & distributed systems — at a glance

*Full method, instruments and caveats: [infra/k8s/RESULTS.md](../infra/k8s/RESULTS.md). Deep version: [docs/study/28-kubernetes-orchestration.md](../docs/study/28-kubernetes-orchestration.md).*

The stateless tier of an end-to-end-encrypted mail platform, running on a 4-node Kubernetes
cluster. The orchestration is the visible part; the harder part is that **multiple replicas must
never print the same letter twice**.

## What was tested, and what happened

| Scenario | Result |
|---|---|
| **Rolling update** while traffic keeps flowing | All 3 API pods replaced in **21 s**, **0 failed requests out of 996** |
| **Autoscaling** under load (k6) | **2 → 7 pods** automatically; p95 latency **10.79 ms → 5.80 ms**, 0 % errors |
| **Scaling back down** after load stops | Automatic, **283 s** later — deliberately damped so a traffic dip can't cause an outage |
| **Killing the pod** holding the printer's live connection | Printer reconnected to a **different** pod in ~1 s; queued jobs still delivered, **none lost, none printed twice** |
| **Node drain / disruption budget** | Kubernetes **refused** the eviction that would have breached availability; the drain itself finished in **8 s** |
| **Crash mid-job** | A job orphaned by a dead node was recovered by another node and delivered **exactly once** |

## The correctness underneath it

Autoscaling a tier that isn't safe to replicate just multiplies the bug. Three mechanisms make the
replica count free to change, and all three predate the cluster:

- **Exactly-once dispatch across nodes** — a Redis Streams consumer group, so a job is delivered to
  one node; if that node dies mid-job the entry stays in its pending list and another node reclaims
  it only once it is *provably* abandoned.
- **A database-level guard** — `SELECT FOR UPDATE NOWAIT` is the authoritative double-dispatch
  block, so correctness never depends on the queue alone.
- **Draining rather than dying** — on shutdown a node finishes in-flight requests, closes live
  status streams deliberately, and deregisters its queue consumer instead of leaking it.

## Stack

Kubernetes (k3d/k3s, Kustomize base + overlays, HPA, PodDisruptionBudget, Traefik ingress,
StatefulSets, downward API, readiness/liveness split) · Go · PostgreSQL · Redis Streams ·
MinIO (S3) · mTLS on every internal hop · k6

## How the numbers were produced

Every figure above was **written into [RESULTS.md](../infra/k8s/RESULTS.md) by the scripted run
that produced it** — `make k8s-failure` and `make k8s-load` — not typed in afterwards. Each run is a
pass/fail gate, so a regression breaks the build rather than quietly changing a number. The
autoscaling run measures against its own single-replica control on the same cluster, minutes apart,
rather than against a number imported from a different topology.

## What these numbers are not

A **4-node local cluster on one developer machine**. Scheduling, rolling updates, eviction, drain
and autoscaling are genuinely exercised — the same Kubernetes control loops as anywhere — but this
is not a production SLA, and the database tier is deliberately single-node. The single replica in
the load test was never saturated, so the autoscaling result shows proportional response, not
rescue from collapse. Those limits are written out in full in
[RESULTS.md](../infra/k8s/RESULTS.md) as part of the deliverable.

## Where to look

| | |
|---|---|
| Manifests | [`infra/k8s/base/`](../infra/k8s/base/), overlays in [`infra/k8s/overlays/`](../infra/k8s/overlays/) |
| The runs that measure it | [`scripts/k8s/`](../scripts/k8s/) (`make k8s-failure`, `make k8s-load`) |
| Cross-node dispatch | [`services/cloud/dispatch/`](../services/cloud/dispatch/), [`services/cloud/link/`](../services/cloud/link/) |
| Design doc | [`plans/16-kubernetes.md`](../plans/16-kubernetes.md) |
