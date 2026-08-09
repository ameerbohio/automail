# Orchestrating the stateless tier: Deployments, probes, rollouts, and the HPA

*Manifests: `infra/k8s/base/`, `infra/k8s/overlays/k3d-local/`. Plan: `plans/16-kubernetes.md` §3, §5, §7, §8. Acceptance: `make k8s-cloud-check`, `make k8s-failure`, `make k8s-load`. **Every number below is from [`infra/k8s/RESULTS.md`](../../infra/k8s/RESULTS.md)**, written by those runs at the moment they ran.*

Companion notes: [25](25-k3d-cluster-image-supply.md) (the cluster and getting images into it), [26](26-k8s-state-and-secrets.md) (StatefulSets, volumes, Secrets), [27](27-printer-dial-in-outside-the-cluster.md) (the device that stayed outside), [24](24-graceful-shutdown-consumer-lifecycle.md) (the Go side of a clean termination — this note is what consumes it).

## What it is

cloud-server and the portal run as Kubernetes **Deployments** on a four-node k3d cluster; Postgres, Redis and MinIO run as single-replica **StatefulSets**; the printer stays outside entirely. Traffic enters through the cluster's bundled Traefik, and a **HorizontalPodAutoscaler** sizes the cloud tier between 2 and 8 replicas.

The honest framing, and the one to lead with: **Kubernetes did not make this system scale.** The tier was already stateless and already coordinated through Redis before any manifest existed — that is what made it schedulable. What the orchestrator added is *operational*: rolling updates that do not drop requests, a scheduler that spreads replicas, a budget that refuses disruptive evictions, and a control loop that reacts to load without me.

## The distinction everything follows from: Deployment or StatefulSet

The test is not "does it store data". It is: **would a second identical copy be an improvement or a defect?**

- cloud-server: a second copy is more capacity. Every replica is interchangeable because the state lives in Postgres, Redis and MinIO — and, crucially, because two replicas racing on the same job is already impossible without Kubernetes: the Redis Streams consumer group makes dispatch exactly-once across nodes ([14](14-redis-streams-consumer-groups.md)) and `SELECT FOR UPDATE NOWAIT` is the authoritative double-dispatch guard ([15](15-select-for-update-nowait.md)). **Deployment.**
- Postgres: a second copy is a split brain. It needs stable identity, stable storage, and ordered startup. **StatefulSet** — which buys identity and storage, *not* availability. One replica with one PVC is still one point of failure, and saying so out loud is worth more than the manifest.
- The printer: a second copy prints the letter twice ([27](27-printer-dial-in-outside-the-cluster.md)). Its replica count is pinned at one by physics. **Neither** — it is not a workload.

A Deployment is really two objects: it owns **ReplicaSets**, and a ReplicaSet owns Pods. That indirection is the whole rolling-update mechanism — an update creates a *new* ReplicaSet and shifts replicas from the old one to the new one, which is why a rollback is instant (the old ReplicaSet still exists, scaled to zero).

## Readiness and liveness answer different questions

This is the probe mistake that looks like diligence and behaves like an outage.

| Probe | Question | Ours | Failure action |
|---|---|---|---|
| **Readiness** | Should this pod receive traffic *right now*? | `/healthz` — checks Postgres and Redis | Removed from Service Endpoints. Reversible. |
| **Liveness** | Is this process wedged and beyond saving? | `/livez` — process only, touches no dependency | **Killed and restarted.** |

Putting the dependency-checking `/healthz` on liveness is the trap: a five-second Redis blip fails the probe on *every* replica simultaneously, so Kubernetes restarts the entire tier — converting a dependency wobble into a total outage, at the exact moment the dependency is struggling and least able to absorb a thundering herd of reconnects. Readiness is the probe that *should* care about dependencies, because its remedy (stop sending traffic) is proportionate and self-reversing.

`/healthz` also returns **503 `DRAINING`** once shutdown begins, which is how readiness participates in a graceful exit: the pod pulls itself out of Endpoints the moment SIGTERM lands, before it stops serving. `/livez` deliberately stays 200 while draining — a correct, deliberate drain must never look like a wedged process, or the kubelet SIGKILLs it mid-drain and every guarantee below becomes theatre.

## What actually happens when a pod goes away

Deleting a pod (or rolling one, or evicting one) starts this, and the ordering is the part worth knowing:

1. The pod is marked `Terminating`. **Two things now happen concurrently, not in sequence:** the kubelet begins the shutdown, *and* the endpoint controller removes the pod from the Service's Endpoints. That removal propagates asynchronously to kube-proxy and to Traefik.
2. **`preStop: sleep 5`** runs first, before SIGTERM. It exists purely to let the endpoint removal win that race. Without it the process starts draining while the load balancer is still handing it new requests — the classic "we drain gracefully and still drop requests" bug.
3. **SIGTERM** to PID 1, and the Go side takes over ([24](24-graceful-shutdown-consumer-lifecycle.md)): drain signal → dispatcher loop → Redis consumer-group cleanup → printer sockets closed with `StatusGoingAway` → public listener → internal listener.
4. At **`terminationGracePeriodSeconds: 35`** the kubelet sends SIGKILL. That number is arithmetic, not a round figure: `5` (preStop) + `20` (`SHUTDOWN_TIMEOUT`) + 10 slack. **If that inequality ever stops holding, the drain is cut off mid-flight** — SSE streams severed instead of closed, the Redis consumer leaked instead of removed.

Measured: after deleting the pod holding the printer's socket, the last subscriber on that mailbox's dispatch channel disappeared **10.6 s** later. That is the budget being spent deliberately, not a hang — which is exactly why it is worth being able to explain rather than "optimise".

And the rollout property that Compose structurally cannot have: `maxUnavailable: 0` with `maxSurge: 1` means a *new* pod must pass its readiness probe before an *old* one is allowed to go away. Measured: `kubectl rollout restart` replaced all three pods in **21 s** under continuous traffic — **996 requests at ~40 req/s, 0 non-2xx, 0 transport errors**, answered by 6 distinct pods. `docker compose up -d --force-recreate` has no surge capacity, no readiness gate and no endpoint propagation; it stops the container.

## The HPA: what "60%" is a percentage of

`averageUtilization: 60` is **not** 60% of a core and **not** 60% of the limit. It is 60% of the pod's CPU **`request`**. Ours requests `100m` and limits `500m`, so the HPA acts when the average pod exceeds ~60m — while the pod is still free to burn five times that before being throttled.

That is the single most useful thing to know about an HPA, because it makes the request a *scaling policy*, not just a scheduling hint:

- request too large → utilization never crosses the target → it never scales, and the tier just gets slower;
- request too small → idle noise pins it at `maxReplicas` and the cluster fills with pods doing nothing;
- and `maxReplicas × request` must still fit the cluster, or the demo ends in `Pending` pods. Ours: 8 × 100m = 800m, 8 × 128Mi = 1Gi.

The loop itself: metrics-server scrapes kubelets (~15 s), the HPA controller syncs (~15 s), and `desiredReplicas = ceil(currentReplicas × currentUtilization / targetUtilization)`. So there is a **30–45 s reaction lag before any scaling happens at all** — measured here as the controller reading 25% → 67% → 124% of target across ~35 s before it acted. A load test with one 65-second stage would show nothing and read as "the HPA is broken".

Measured, under three waves of identical load: **2 → 7 replicas**, peak **124%** of target, and worst-case p95 **10.79 ms at one replica → 5.80 ms autoscaled** at ~88 req/s with 0% errors. `maxReplicas: 8` was deliberately *not* reached — the controller sized to the load rather than slamming into the ceiling, which is what the formula predicts and a nice thing to have on record.

## Why scale-up and scale-down are deliberately asymmetric

`scaleUp` has a **0 s** stabilization window; `scaleDown` has **300 s**. During the scale-down window the controller uses the **maximum** recommendation across the trailing five minutes, so replicas only fall once no busy sample remains in it.

The asymmetry is a cost argument, not a config default:

- Scaling up late is **user-visible latency** during the window you are least able to afford it. Scaling up early costs a few pod-minutes.
- Scaling down early costs an outage on the next spike — and it removes precisely the replicas that were absorbing it. Scaling down late costs a few pod-minutes.
- Flapping is not free either: every removed replica spends the full drain budget on the way out and takes its Redis consumer registration with it.

Measured: first scale-down **283 s** after the load stopped, `minReplicas` at 299 s. Slightly *under* 300 s because the window is anchored to the last elevated **recommendation the controller stamped** — computed shortly before the final requests drained — not to the moment traffic stopped. A small detail, but being able to explain a number that looks off by 17 seconds is worth more than the number itself.

## Two objects both trying to own `replicas`

`deployment.yaml` says `replicas: 3` and the HPA also writes that field. Whoever wrote last wins, so `kubectl apply -k` snaps the tier to 3 and the HPA moves it back within a sync or two. Keeping the number in git is the deliberate choice here — it documents the design size and keeps a cluster-less `kubectl kustomize` render honest — but the GitOps answer is to **drop `replicas` from the Deployment entirely** once a reconciler is re-applying continuously, or the two of them will fight forever.

There is a second collision worth naming because it is the kind of thing that is only found by running it: `minReplicas: 2` meets a PodDisruptionBudget of `minAvailable: 2`. At the HPA's floor the budget therefore permits **zero** voluntary disruptions, and a `kubectl drain` blocks until something scales the tier up. That is the PDB doing its job — it refuses to trade away the last redundant replica for an administrator's convenience — but two individually-reasonable numbers produced an operationally awkward one, and the fix (floor of 3, or `minAvailable: 50%`) is a real trade-off rather than an obvious bug.

The PDB, measured: `currentHealthy 3 / desiredHealthy 2 / disruptionsAllowed 1`; the first eviction succeeded, the second was refused with `TooManyRequests: Cannot evict pod as it would violate the pod's disruption budget`, and a full node drain completed in **8 s** with the tier intact. Note that a PDB governs the **eviction API** only — `kubectl delete pod` bypasses it entirely, which is the difference between voluntary and involuntary disruption.

## What this does not prove — volunteer these first

- **Four k3d nodes are four containers on one kernel, one Docker daemon, one disk.** Scheduling, anti-affinity, eviction, drain and rolling updates are genuinely exercised, because Kubernetes runs the same control loops here as anywhere. Correlated hardware failure, real network partitions and multi-host scheduling are not.
- **The data tier is three single points of failure** and is the real scaling frontier ([`plans/15-v3-roadmap.md`](../../plans/15-v3-roadmap.md)). Scaling the stateless tier moves the bottleneck *toward* them.
- **Autoscaling on CPU is the metric that was available, not the metric that is right.** Queue depth or in-flight requests would be better signals for this workload; that needs a metrics adapter, which proves nothing further about the design.
- **The load test never saturated the single replica** (0% errors, ~327m of a 500m limit), so the HPA run demonstrates a proportional response, not a rescue from collapse.
- **No service mesh, no NetworkPolicy, no cert-manager.** Pod-to-pod traffic inside the cluster is unrestricted; the mTLS invariant is enforced at the application layer, as it always was. A default-deny NetworkPolicy is the honest next step.

## Interview one-liners

- *"Deployment or StatefulSet?"* — Ask whether a second identical copy is capacity or a bug. Capacity → Deployment. Bug → StatefulSet, or not a workload at all.
- *"Readiness vs liveness?"* — Readiness gates traffic and may depend on dependencies; liveness restarts the process and must not. A dependency check on liveness turns a Redis blip into a full-tier restart.
- *"Why a preStop sleep?"* — Endpoint removal and SIGTERM are concurrent. The sleep lets the load balancer find out before the process starts refusing work.
- *"60% of what?"* — Of the CPU **request**, not the limit and not a core. The request is therefore a scaling policy.
- *"Why is scale-down five minutes and scale-up instant?"* — Because the two mistakes cost different things: scaling up late is user-visible latency, scaling down early is an outage on the next spike.
- *"So Kubernetes made it scale?"* — No. It was stateless and coordinated through Redis Streams before any manifest existed; that is *why* it could be scheduled. Kubernetes gave me rolling updates with zero dropped requests, which Compose structurally cannot do.
