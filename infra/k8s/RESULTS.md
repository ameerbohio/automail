# Kubernetes track — measured results

*Skimming? [00-PROJECT-SUMMARIES/kubernetes.md](../../00-PROJECT-SUMMARIES/kubernetes.md) is the one-page version.
This file is the evidence behind it — method, instruments, and the limits of each claim.*

What the cluster actually did, recorded at the moment it did it. Goal K8 turns
these into resume bullets, and a bullet whose number cannot be traced back to a
committed run is a bullet that will not survive an interview.

**Read the caveats before the numbers.** This is a four-node k3d cluster: four
containers on one WSL2 kernel, one Docker daemon, one disk. Node anti-affinity,
eviction, drain and rolling-update behaviour are genuinely exercised, because
Kubernetes runs the same control loops here as anywhere. Correlated hardware
failure, real network partitions, kernel-level isolation and multi-host
scheduling are **not** exercised and no line below should be read as if they
were. See `plans/16-kubernetes.md` §8.

The blocks between the `BEGIN`/`END` markers are written by the runs themselves
— editing them by hand defeats the purpose. The prose around them is authored.

---

## Goal K6 — failure and rollout behaviour

Command: `make k8s-failure` (`scripts/k8s/failure-check.sh` → `tests/system/k8s_failure_test.go`,
build tag `k8sfail`). It runs against a cluster brought up by `make k8s-up &&
make k8s-images && make k8s-secrets && make k8s-apply`, with the printer outside
the cluster in its own Compose project, exactly as Goal K5 left it.

### What each scenario is for

| Scenario | The claim | Why Compose could not make it |
|---|---|---|
| `socket_owner_deleted` | The printer's mTLS socket **fails over** to a different pod, and jobs submitted while no socket exists anywhere are queued, not lost. | The Compose printer dials a fixed service alias, so the socket can only ever come back on the same name — the survivor could buffer work but never take the socket. Goal T9 recorded that as an honest boundary. |
| `pel_holder_deleted` | A stream entry orphaned in a dead pod's Pending Entries List is recovered by **`XAUTOCLAIM`** on a survivor and delivered exactly once. | The crash-recovery path had only ever been shown in the T5 Redis integration test, never end-to-end through the assembled product. |
| `pdb_and_node_drain` | A `PodDisruptionBudget` of `minAvailable: 2` permits one eviction and **refuses** the second; a full `kubectl drain` of an agent node completes with the tier intact. | Compose has no concept of voluntary disruption, eviction, or scheduling. |
| `rolling_update` | Every pod in the tier is replaced under continuous traffic with **zero non-2xx**. | `docker compose up -d --force-recreate` has no surge capacity, no readiness gate and no endpoint propagation — it just stops the container. |

### Three instruments, declared

- **The printer is `docker pause`d** across the socket-failover window. Its
  reconnect backoff is ~1 s, so the window in which no pod holds its socket is
  about a second wide; racing HTTP submissions against it would make "queued,
  not dispatched" a coin flip. Pausing widens the window deterministically. The
  unpause is a real reconnect — the process wakes to a dead TCP peer and
  redials.
- **The printer is `docker stop`ped, not paused, for the reclaim scenario**, and
  the difference is worth stating because the first attempt got it wrong: a
  paused container keeps its TCP connection open, so the cloud pod kept the
  socket and stayed *subscribed* — dispatch went on succeeding into a process
  that could not answer. A paused peer is not a disconnected peer, and TCP
  cannot tell them apart; only the far end closing does. Pausing suffices in the
  failover scenario solely because what removes the subscriber there is the pod
  being deleted.
- **Jobs are pre-encrypted and pre-uploaded** before the kill, so the only call
  landing inside the outage window is a single `POST /jobs` whose returned
  status is the assertion.

No instrument weakens a claim: nothing about the cloud tier, the dispatch path
or the printer's own logic is modified or stubbed.

<!-- BEGIN K6 MEASUREMENTS -->

Measured by `make k8s-failure` (`scripts/k8s/failure-check.sh` →
`tests/system/k8s_failure_test.go`), all four scenarios passed.

- Run: `2026-08-09T02:29:51-04:00` against k3d cluster `automail`, k3s `v1.33.13+k3s2`.
- Socket owner `cloud-server-648788c45b-rt4vb` deleted; the last subscriber on `mailbox:00000000-0000-0000-0000-000000000001:dispatch` disappeared **10.6s** after the delete (preStop 5s + graceful drain, so this is the shutdown budget being spent, not a hang).
- 2 jobs submitted through the ingress during that window were accepted as `queued` and parked in `jobs:pending` (depth 21): with no live socket anywhere, `attemptDispatch` publishes, sees 0 receivers, reverts the Postgres claim and re-queues rather than reporting progress.
- The printer redialled `wss://localhost:9843` and was answered by **`cloud-server-648788c45b-thmx6`** (was `cloud-server-648788c45b-rt4vb`), 1s after the unpause. This is the property Compose cannot have: there the printer dials a fixed service alias, so the socket can only come back on the same name.
- Both queued jobs reached `delivered` after the failover, each with exactly one `job_delivered` row in the append-only `audit_events` ledger (0 would mean lost, 2 would mean double-printed), and the printer's `/dev/shm` was empty afterwards.
- The Deployment returned to 3/3 Ready on its own; the ReplicaSet replaced the deleted pod.
- A job queued with no printer available was read by pod **`cloud-server-648788c45b-l5cxw`** after 26s and left un-ACK'd in its PEL -- `handle()` leaves a still-blocked entry pending rather than re-`XADD`ing it, so the entry is never multiplied.
- Deleting `cloud-server-648788c45b-l5cxw` left its consumer in the `dispatchers` group holding 1 pending entry: graceful shutdown skips `XGROUP DELCONSUMER` while the PEL is non-empty, because DELCONSUMER discards pending entries instead of returning them (the Goal K0 trap, guarded here for real).
- After the printer reconnected, the orphaned entry was invisible to every survivor's `XREADGROUP >`; **`cloud-server-648788c45b-thmx6`** recovered it via `XAUTOCLAIM` 1m2s later (log: `2026/08/09 06:31:33 dispatch: reclaimed job from crashed consumer: 1786257005063-0`) and the job reached `delivered` with exactly one ledger row. The delay is `claimMinIdle` (60s) plus the sweep period, not latency -- an entry must be *provably* abandoned before another node may take it, or two nodes would print the same letter.
- `PodDisruptionBudget/cloud-server` before the drain: currentHealthy 3, desiredHealthy 2, disruptionsAllowed 1.
- With one pod already disrupted, `disruptionsAllowed` fell to 0 and the API server **refused** the second eviction: `Error from server (TooManyRequests): Cannot evict pod as it would violate the pod's disruption budget.`. Kubernetes declined to trade availability for convenience; nothing in the application had to know.
- `kubectl drain k3d-automail-agent-0 --ignore-daemonsets --delete-emptydir-data` completed in 8s while the PDB held. Pod placement went from `k3d-automail-agent-0=1 k3d-automail-agent-2=1 k3d-automail-server-0=1` to `k3d-automail-agent-1=1 k3d-automail-agent-2=1 k3d-automail-server-0=1`: the evicted replica rescheduled onto a remaining node because the anti-affinity is **preferred**, not required -- a required rule would have turned this drain into an outage. The node was chosen to be neither the server node (the data tier's `local-path` PVs are pinned there) nor the node running Traefik (draining that moves the ingress every other scenario measures through).
- The node was uncordoned afterwards; nothing else was left cordoned.
- `kubectl rollout restart deploy/cloud-server` replaced all 3 pods in 21s under continuous traffic (996 requests at ~40 req/s through the Traefik ingress to `/healthz` on `api.automail.local`): **0 non-2xx, 0 transport errors**, answered by 6 distinct pods (`X-Automail-Node`). The traffic uses the catch-all router deliberately -- the guest rate-limited router would have returned 429s that read as dropped requests.
- After the rollout the `dispatchers` group settled to one consumer per live pod (3), the Goal K0 `XGROUP DELCONSUMER` guard doing its job: `NODE_ID` is the pod name, so without it every `rollout restart` would leak three consumers forever. Residual consumers: [cloud-server-648788c45b-l5cxw] — pods this run killed deliberately, left for the idle reaper (they are not leaks, and deleting one while it held pending entries is precisely what must not happen). The check waits for convergence: `rollout status` returns before the outgoing pods have spent their termination grace, and a consumer that is still mid-exit is not a leak.

<!-- END K6 MEASUREMENTS -->

### What this does *not* prove

- **The data tier is still three single points of failure.** One Postgres, one
  Redis, one MinIO, each with one PVC, all pinned to the server node by the
  k3d-local overlay. A `StatefulSet` with `replicas: 1` buys stable identity and
  storage, not availability. That is the V3 frontier (`plans/15-v3-roadmap.md`),
  not something a drain demo addresses.
- **The drained node was an agent, deliberately.** `local-path` PVs carry a
  nodeAffinity to the node they bound on, so draining the server node would
  strand Postgres in `Pending` forever. That is a property of node-local
  storage, not a Kubernetes limitation — but it does mean the drain scenario
  never moved a stateful workload.
- **`XAUTOCLAIM` recovery is bounded by `claimMinIdle` (60 s), by design.** The
  reclaim delay measured above is not latency to be optimised away: an entry has
  to be *provably* abandoned before another node may take it, or two nodes print
  the same letter. The user-visible path (a live printer, a live socket) is
  unaffected — this is the path that only runs after a pod dies mid-dispatch.
- **Zero dropped requests was measured on `/healthz`, not on job submission.**
  The guest-rate-limited routers would have answered 429 under sustained
  traffic, and a 429 would read as a dropped request. The rollout property being
  tested is endpoint propagation and drain ordering, which is path-independent;
  throughput under load is Goal K7's measurement, not this one.

---

## Goal K7 — HPA under k6 load

Command: `make k8s-load` (`scripts/k8s/load-check.sh` → `scripts/load/k8s-report.py`),
against the same cluster the other K-goals use.

### What is being measured, and against what

The claim is **"the tier autoscales under load on my dev host"**, and the only
way that claim survives contact with an interviewer is if the control is stated
first. There are two candidate controls and they are not equivalent:

| Control | What it is | Why it is / is not the gate |
|---|---|---|
| `scripts/load/baseline.json` (Goal T10) | The committed Compose baseline. | **Context, not pass/fail.** It was measured on a single cloud-server container with **no CPU limit**, on the Compose network. Every pod here has a request *and* a 500m limit, so the topology differs in the one dimension the measurement is about. Quoting it as a gate would be comparing two different systems. |
| The single-replica reference run | The same load, on this cluster, with the HPA deleted and `replicas: 1`. | **This is the gate.** Same kernel, same images, same limits, minutes apart — the only variable left is the replica count. |

### Four instruments, declared

- **The load generator runs inside the cluster.** k6 is not installed on this
  host, and a host-side run would have to come in through the Traefik edge,
  adding TLS termination, an ingress hop and the guest rate limit to the
  measured path — none of which the Compose baseline contains. The k6 pods hit
  the `cloud-server` ClusterIP Service directly, exactly as the Compose k6
  container hit `cloud-server:8080`.
- **`scripts/load/submission.js` is unmodified**, byte-for-byte the file `make
  load` runs. One k6 pod peaks at roughly 110m of cloud-server CPU here, which
  against a 100m request is not enough to cross a 60% target at two replicas, so
  the offered load is multiplied by running the *same* script in several pods
  rather than by editing its stages. Editing the stages would have made the
  Compose numbers incomparable in exactly the way this whole section is trying
  to avoid.
- **The load profile overrides `MINIO_PUBLIC_ENDPOINT` to empty**
  (`infra/k8s/overlays/k3d-load`), so pre-signed upload URLs are signed for the
  in-cluster `minio:9000` rather than the browser-facing edge hostname. Without
  it every iteration fails at the PUT against a name no pod can resolve.
  `infra/compose/load.yml` sets the identical empty value for the identical
  reason. The overlay is reverted at the end of the run, including on abort.
- **No printer is running.** Every submission therefore takes the *queued* path
  (`XADD` to `jobs:pending`) rather than immediate dispatch. This is stated
  because it differs from the Compose baseline, where a `DEV_MODE` printer was
  up: it is chosen because a single dev printer has five slots, so under
  sustained submission rates some fraction of jobs would queue anyway and the
  measured path would become a non-deterministic mix. One path for every
  iteration makes the two phases comparable to each other, which is what the
  gate above requires.

<!-- BEGIN K7 MEASUREMENTS -->

Measured by `make k8s-load` (`scripts/k8s/load-check.sh` → `scripts/load/k8s-report.py`) on **2026-08-09T13:05:36-04:00**, k3s `v1.33.13+k3s2`, against the four-node k3d cluster on this WSL2 dev host. Load is `scripts/load/submission.js` **unmodified** — the same file `make load` runs on Compose — driven from **3 in-cluster k6 pods per wave**, 3 waves per phase.

HPA: `minReplicas 2`, `maxReplicas 8`, target **60% of the CPU request** (request `100m`, limit `500m`) — so the trigger is utilization of the *request*, not of a core, and not of the limit.

**Single-replica reference** (HPA deleted, `replicas: 1`) — the control this run is judged against:

| Wave | Replicas during wave | Peak CPU vs target | Submissions | Offered rate | p95 (worst generator) | Errors |
|---|---|---|---|---|---|---|
| 1 | 1 | — | 5436 | 88.4/s | 10.79 ms | 0.00% |
| 2 | 1 | — | 5436 | 88.3/s | 5.60 ms | 0.00% |
| 3 | 1 | — | 5436 | 88.4/s | 9.59 ms | 0.00% |

**Autoscaled** (identical load, HPA in charge):

| Wave | Replicas during wave | Peak CPU vs target | Submissions | Offered rate | p95 (worst generator) | Errors |
|---|---|---|---|---|---|---|
| 1 | 2 → 5 | 124% | 5436 | 88.4/s | 5.56 ms | 0.00% |
| 2 | 5 → 6 | 124% | 5436 | 88.4/s | 5.80 ms | 0.00% |
| 3 | 6 | 59% | 5436 | 88.4/s | 5.64 ms | 0.00% |

- Worst p95 across all waves: **10.79 ms at one replica → 5.80 ms autoscaled** (−46%); worst error rate 0.00% → 0.00%. Same script, same offered load, same cluster minutes apart — the only variable is how many replicas were serving it.
- Replica count over the run: floor 1 → peak **7** (HPA spec peak 7, `maxReplicas` 8 was not reached — the controller sized to the load rather than slamming into the ceiling).
- Peak measured CPU utilization: **124%** of the 60% target.
- **Scale-down began 283s after the load stopped** and reached `minReplicas` at 299s. That delay is `scaleDown.stabilizationWindowSeconds: 300` being waited out, not sluggishness: the controller takes the *maximum* recommendation across the trailing five minutes, so replicas only fall once no busy sample remains in the window. Scale-up has a 0s window by contrast — under-capacity is user-visible, over-capacity costs a few pod-minutes.
<!-- END K7 MEASUREMENTS -->

### Context: the Compose baseline, quoted as context and not as a gate

`scripts/load/baseline.json` recorded **p95 7.41 ms at 28.63 req/s over 1812
iterations** on one unlimited Compose container. Because `submission.js` uses a
`ramping-arrival-rate` executor, its iteration count is deterministic — each k6
pod above ran the same **1812** iterations, so the per-generator workload is
literally identical and only the topology differs. Three generators against one
*limited* cluster replica (~88 req/s offered, p95 10.79 ms) is therefore roughly
three times the Compose offered rate at ~1.5× its p95 — the direction one would
expect from a pod capped at 500m CPU, and the reason the cluster's own
single-replica run, not this number, is the gate.

### What this does *not* prove

- **The single replica was never saturated, so this is a proportionality demo,
  not a rescue.** At one replica the tier still answered every request — 0%
  errors, p95 10.79 ms — while burning ~327m of its 500m CPU limit. The HPA
  scaled because utilization of the *100m request* was over 60%, which is a
  deliberately conservative trigger, not because the tier was failing. A demo
  that showed latency collapsing at one replica and recovering at seven would be
  a stronger story; it would also have required either a heavier generator than
  this host can run beside the cluster, or a request sized to lie.
- **This is not a throughput claim.** "Autoscaled 2→N pods under k6 load on a
  WSL2 dev host" is what the numbers support. "Handles N req/s in production" is
  not, and nothing here should be quoted that way: the load generators, all four
  Kubernetes nodes, Postgres, Redis and MinIO share one kernel, one Docker
  daemon and one disk, so the generator and the system under test compete for
  the same cores.
- **The HPA scaled on CPU only.** Requests-in-flight, queue depth or
  submission latency would be better signals for this workload, and are what a
  production autoscaler would use (`external`/`pods` metrics via an adapter).
  CPU is what metrics-server serves out of the box, and adding an adapter proves
  nothing further about the design.
- **The data tier did not scale and cannot.** One Postgres, one Redis, one MinIO,
  each with one PVC pinned to the server node. Scaling the stateless tier moves
  the bottleneck *toward* them, which is the honest reading of any headroom
  measured above — and the reason `plans/15-v3-roadmap.md` exists.
- **The scale-up was measured, the failure of a scaled-out tier was not.** Goal
  K6 covers pod loss and rollout behaviour at three replicas; nothing here
  re-runs those scenarios at eight.
