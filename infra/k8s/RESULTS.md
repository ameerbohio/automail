# Kubernetes track — measured results

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

Command: `make k8s-failure` (`scripts/k8s/failure-check.sh` → `e2e/k8s_failure_test.go`,
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
`e2e/k8s_failure_test.go`), all four scenarios passed.

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
