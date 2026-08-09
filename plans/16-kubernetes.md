# Kubernetes Orchestration (demo)

> **Scope.** Replace the Docker Compose deployment of the **stateless tier** — cloud-server
> and portal — with a Kubernetes Deployment each, scheduled across a multi-node local
> cluster, load-balanced by an Ingress, and autoscaled by an HPA under real load. The
> printer stays exactly where the architecture says it lives: **outside** the cluster, in
> the physical mailbox unit, dialing in through NAT over mTLS.
>
> This is the item `plans/03-scaling.md` "Production Scaling Path" lists as *"Docker Compose
> `--scale` → Kubernetes Deployment, HPA"*, and the orchestration half of
> `plans/13-v2-roadmap.md` "Multi-node fleet topology". It is promoted here from *noted, not
> built* to a built demo.

---

## 1. Why this is cheap, and what that means for how you talk about it

**The distributed work is already done.** The cloud tier holds no authoritative in-process
state (`plans/03-scaling.md` §"Stateless Server Contract"): job records in Postgres, queue in
Redis Streams with a consumer group, dispatch fan-in and SSE fan-out over Redis pub/sub. Any
replica can serve any request and dispatch to any printer.

Measured on the dev host, 2026-08-06, before any Kubernetes work existed:

| Check | Result |
|---|---|
| `--scale cloud-server=3` behind Traefik | 9 requests spread evenly over 3 distinct `X-Automail-Node` values |
| `XINFO GROUPS jobs:pending` | all 3 joined the `dispatchers` consumer group |
| `--scale portal=2` | both replicas served 200 through the edge |
| `--scale printer=2` | `PUBSUB NUMSUB mailbox:<id>:dispatch` → **2** — one dispatch reaches both printers |

So Kubernetes is **not** what makes this system scale. It is a *scheduler and a control loop*
sitting on top of a tier that already scaled. Say that out loud in an interview — claiming
"Kubernetes made it scalable" invites the follow-up "how does a second replica know about the
first?", and the honest answer (Redis pub/sub + a Streams consumer group, proven by a
two-hub test in `services/cloud/stream_test.go`) is far stronger than the k8s answer.

**What Kubernetes genuinely adds** — and these are the defensible claims:

- **Declarative desired state + self-healing.** Kill a pod, the ReplicaSet rebuilds it. Compose
  cannot do this.
- **Scheduling across a real failure domain.** Replicas land on different nodes; anti-affinity
  makes that a guarantee rather than luck.
- **Rolling updates with zero dropped requests** — readiness gating + a PodDisruptionBudget +
  graceful shutdown, which Compose has no vocabulary for.
- **Autoscaling from an observed signal** (HPA on CPU, driven by the existing k6 script).
- **Config/secret separation from images** — ConfigMap and Secret instead of a bind-mounted
  `.env` and a host `./infra/certs` mount.

---

## 2. Substrate: k3d, one server + three agents

**Decision: k3d** (k3s running as containers in the Docker daemon that already works on this
host). Reasons, in priority order:

1. **It ships Traefik as the built-in ingress controller.** This project's routing is already
   Traefik; the compose labels translate to Traefik `IngressRoute` CRDs almost line for line,
   including the existing `secure-headers` and `guest-ratelimit` middleware. kind or minikube
   would mean installing an ingress controller *and* re-learning its annotation dialect.
2. **Multi-node for free.** `--agents 3` gives four nodes on one host, so pod anti-affinity and
   "drain a node, watch pods reschedule" are real demonstrations, not simulations. The host has
   24 cores / 15 GB — comfortable.
3. **No registry needed.** `k3d image import` pushes locally built images straight into the
   cluster, so the existing `docker build` stays the build path.

Alternative noted and rejected: kind (no bundled ingress, no image-import ergonomics), minikube
(heavier VM, slower cycle). If this ever moves to the Proxmox host, k3s proper is the same
control plane without the Docker wrapper — the manifests carry over unchanged.

### 2.1 Preflight: what this host actually has (measured 2026-08-07)

The §1 measurements were taken on the Compose stack and say nothing about whether a cluster can
boot here. These do. **Check every row before writing a manifest** — three of them can stop the
track dead.

| Check | Measured | Consequence |
|---|---|---|
| `command -v k3d` / `kubectl` | **both absent** | Neither is installed. K1 installs them (pinned versions) or the track cannot start. **Settled by K1**: `k3d v5.9.0` + `kubectl v1.33.13`, checksum-verified into `~/.local/bin` by `scripts/k8s/tools.sh` (no sudo — this host has no passwordless sudo). |
| `docker version` | Engine 29.6.1 | Fine — k3d needs a working Docker daemon and this is the same one every Compose target uses. |
| `stat -fc %T /sys/fs/cgroup` | **`tmpfs`** → **cgroup v1** | **Was the blocking risk; measured clear by K1 (2026-08-08).** k3s **v1.33.13-k3s2** registered all 4 nodes `Ready` on this cgroup-v1 (hybrid) kernel, with CoreDNS, metrics-server, local-path-provisioner, Traefik and klipper-lb all `Running`. The `/etc/wsl.conf` + `wsl --shutdown` owner action was **not** needed. This is version-bound, not settled forever: Kubernetes has had cgroup v1 in maintenance mode since 1.31, which is precisely why the k3s pin is 1.33 and not the newest minor. Re-verify before raising it. |
| `nproc` / `free` | 24 cores, 15 GiB total (~10 GiB free) | Four k3s nodes plus the data tier plus 3+2 app pods fit, but not comfortably. Pod `requests` must be budgeted against ~10 GiB, and `maxReplicas: 8` (§7) must still be schedulable or the HPA demo ends in `Pending` pods. |
| `/var/lib/docker` free | 898 GiB | Image imports are not a constraint. |
| ports 80/443 on the host | **held by Windows** (already documented in `.env.example` / `docker-compose.yml`) | The k3d loadbalancer must publish alternate host ports — which cascades into the CSP/CORS/presign chain, see §8.1. **K1 chose 9080/9443** (not 8080/8443, which the Compose stack owns) so the cluster and the Compose stack can run simultaneously — Process Rule 3 in practice. Printer mTLS is `127.0.0.1:9843 → nodePort 30843`; API server `127.0.0.1:6445`. All four are fixed in `infra/k8s/k3d-cluster.yaml`. |
| `/etc/hosts` | **no `*.automail.local` entries** | Every existing suite reaches the edge with `curl --resolve` or by bypassing Traefik entirely. A *browser* (§ the K4 acceptance) needs real hosts entries — another owner/sudo action. |

Two more substrate facts that shape the manifests:

- **Storage is `local-path`** (k3s's default StorageClass): node-local, `WaitForFirstConsumer`.
  A PVC binds to whichever node first schedules its pod and the PV then carries a nodeAffinity
  pinning it there. Deleting a data-tier pod reschedules it back to the same node with its data
  intact (good), but **draining that node leaves it `Pending` forever** (bad — and §5 asks for a
  drain). Pin the data tier to the k3d *server* node and only ever drain agents.
- **Compose declares no `image:` keys** — it builds from `build: ./services/...`, so the images
  are named by the Compose project. The manifests need explicit, deterministic tags
  (`automail/cloud-server:dev`, `automail/portal:dev`) and `imagePullPolicy: IfNotPresent`.
  Do **not** tag `:latest`: Kubernetes forces `Always` on it, and a k3d cluster with no registry
  answers that with `ImagePullBackOff`. Note also that `k3d image import` keys on image ID, so a
  rebuild under the same tag needs a re-import **and** a `kubectl rollout restart` — otherwise
  you are debugging a stale binary.

---

## 3. Object map: what each Compose service becomes

| Compose service | Kubernetes object | Replicas | Why |
|---|---|---|---|
| `cloud-server` | **Deployment** + Service (ClusterIP) + Service (NodePort, mTLS 8443) | 3, HPA 2→8 | Stateless; the whole point of the exercise |
| `portal` | **Deployment** + Service (ClusterIP) | 2 | Stateless Next.js standalone; proxies server-side to the cloud Service |
| `traefik` | **built into k3s** — replaced by `IngressRoute` + `Middleware` CRDs | 1 (cluster-managed) | The LB becomes the platform's job, not a service you run |
| `postgres` | **StatefulSet** + headless Service + PVC | 1 | Stable identity + stable volume. **Not HA** — see §8 |
| `redis` | **StatefulSet** + headless Service + PVC | 1 | Same. Load-bearing for dispatch correctness |
| `minio` | **StatefulSet** + headless Service + PVC + IngressRoute | 1 | Browser-facing (pre-signed PUT), so it keeps its own hostname |
| `printer` | **not a workload** — stays a container on the host | 1 physical unit | See §6 |

**Manifest layout** — Kustomize, not Helm, for the first pass:

```
infra/k8s/
  base/
    kustomization.yaml
    cloud-server/{deployment,service,service-mtls,hpa,pdb}.yaml
    portal/{deployment,service}.yaml
    data/{postgres,redis,minio}-statefulset.yaml  (+ services, PVCs)
    ingress/{ingressroutes,middlewares,tls-secret}.yaml
  overlays/
    k3d-local/     # NodePorts, 1Gi PVCs, DEV_MODE printer, image tags = :dev
    proxmox/       # real hostnames, larger PVCs, replica counts
```

Kustomize is `kubectl`-native (no extra binary), and base+overlay is the pattern that actually
comes up in interviews about environment drift. A Helm chart is a cheap conversion afterwards
(§9, optional) if the literal keyword is wanted on the resume — but ship the working cluster
first.

---

## 4. Code changes that must land before any manifest

These are not Kubernetes concessions; they are latent bugs that Compose's forgiving lifecycle
hid. Each has a Docker-visible symptom today.

### 4.1 Graceful shutdown in cloud-server *(blocking)*

`services/cloud/main.go` ends in `server.ListenAndServe()` + `log.Fatal` with no signal
handling — `services/printer/main.go:25` already does this correctly with
`signal.NotifyContext`, so the pattern is in-repo.

Under Compose, `docker compose down` sends SIGTERM and nobody notices. Under Kubernetes, every
rolling update and every HPA scale-down sends SIGTERM to a pod that is **still receiving
traffic** (Endpoints removal and the TERM signal race). Without a graceful path: in-flight job
submissions are killed mid-request, open SSE streams close without a terminal event, and any
printer socket that pod owned drops uncleanly.

Required: `signal.NotifyContext(SIGINT, SIGTERM)` → `server.Shutdown(ctx)` with a bounded
timeout, and the dispatcher loop cancelled from the same context.

Four things make this more than the three-line version, all verified in the current tree:

1. **`Shutdown` does not cancel in-flight request contexts.** It closes idle connections and
   then *waits* for active ones. `StreamJob` (`services/cloud/handlers/jobs.go:294`) blocks in a
   `select` on `r.Context().Done()`, which fires when the **connection** drops — not on
   shutdown. So every open SSE stream pins `Shutdown` for the whole grace period and is then
   severed anyway: exactly the failure this section exists to prevent. The code comment there
   ("Client went away (or server shutdown)") is aspirational today. Needs an explicit drain
   signal — `server.RegisterOnShutdown` closing a channel that `StreamJob` also selects on — so
   streams end deliberately and fast.
2. **There are two `http.Server`s.** The public one, and `startMTLSServer`
   (`services/cloud/main.go:93`), which is launched in a goroutine, returns only an `error`, and
   ends in `log.Fatal`. It exposes no handle to shut down. Draining the public listener while
   the internal one is killed by process exit leaves the printer link exactly as broken as
   before.
3. **The printer link is a hijacked connection.** `http.Server.Shutdown` neither waits for nor
   closes hijacked conns, so the WebSocket dies with the process and the printer waits out a TCP
   timeout before reconnecting. `link.Hub` has no shutdown path today — only a per-connection
   `defer conn.CloseNow()`. A `Hub.Close()` that sends `websocket.StatusGoingAway` is what makes
   the reconnect prompt.
4. **Cancelling the dispatcher mid-`handle` is not free.** `drain`/`reclaim`/`handle` all take
   the same `ctx`, so cancelling it aborts an in-flight dispatch. Decide deliberately: stop
   *reading* new messages on cancel, but let the current `handle` finish on a detached context —
   otherwise a message can end up neither ACKed nor dispatched at precisely the moment §4.2
   deletes the consumer that owns it.

### 4.2 Consumer-group cleanup on shutdown *(blocking)*

Measured 2026-08-06: **4 consumers registered in the `dispatchers` group for 3 live nodes** —
one stale entry left by a previous container. Compose restarts are rare enough that this is
invisible; a k8s cluster with rolling updates and an HPA churns pods constantly, so the group
grows without bound. Stale consumers are not merely untidy: an entry that dies holding a
pending message keeps that message in its PEL until `XAUTOCLAIM`'s `MinIdle` elapses, which is
**added dispatch latency on every rollout**.

Required: `XGROUP DELCONSUMER jobs:pending dispatchers <nodeID>` on graceful shutdown, after the
dispatcher loop stops and any claimed message is ACKed. A periodic reaper for consumers idle
beyond a threshold covers the ungraceful case (OOMKill, node loss) — belt and braces, since
DELCONSUMER on shutdown only helps when shutdown happens.

**The trap: `XGROUP DELCONSUMER` discards that consumer's PEL entries** — they are dropped from
the group, not handed back for `XAUTOCLAIM` to reclaim. "ACKed **or deliberately left for
reclaim**" is therefore self-contradictory: leaving a message for reclaim and then deleting the
consumer *loses the job*. Both the shutdown path and the reaper must check `XPENDING` for the
consumer first and **skip the delete when the PEL is non-empty** (or `XCLAIM` the entries to a
live consumer before deleting). The reaper's idle threshold must also be stated relative to the
existing `XAUTOCLAIM` `MinIdle` in `services/cloud/dispatch/dispatcher.go`, so a consumer is
never reaped inside the window where reclaim is still expected to fire.

Note the ordering dependency this creates with §4.3: once `NODE_ID` is the pod name, the
consumer name changes on **every** rollout, so a cloud-server Deployment without this cleanup
grows the group by three consumers per rollout from day one.

### 4.3 `NODE_ID` from the downward API *(non-blocking, do it anyway)*

`main.go:224` falls back to `os.Hostname()`, which in Kubernetes is the pod name — correct and
unique already. Set it explicitly from `fieldRef: metadata.name` so the consumer name is a
deliberate contract rather than an accident of the container runtime, and so `X-Automail-Node`
shows a pod name you can `kubectl logs` directly.

### 4.4 Readiness vs liveness split *(non-blocking)*

`/healthz` checks Postgres and Redis connectivity (`plans/03-scaling.md` §Health Checks). That
is the right **readiness** probe. It is the *wrong* liveness probe: a Redis blip would make
Kubernetes restart every cloud pod simultaneously, turning a dependency wobble into a total
outage. Liveness needs a probe that answers "is this process wedged", not "is the world
healthy" — a trivial handler that only proves the mux is serving.

Name it `/livez` (the Kubernetes convention) rather than overloading `/healthz`, register it on
the **public** mux only, and keep it off the Traefik guest router so it is never rate-limited.
`plans/03-scaling.md` §Health Checks documents `/healthz` as the Traefik health check; that role
is unchanged — `/livez` is additive.

**The portal has neither.** `services/portal` exposes no health endpoint and its Dockerfile
carries no `HEALTHCHECK`. A portal Deployment with `maxUnavailable: 0` but no readiness probe
still drops requests during a rollout, because Kubernetes considers a pod Ready the moment the
container starts — before Next.js is listening. Either add a trivial `app/api/healthz` route
(a small code change, and therefore in scope for §4, not for a manifest goal) or probe `GET /`
and accept that it exercises an SSR render on every probe.

---

## 5. Deployment design notes (the details that get asked about)

- **`replicas: 3` and `strategy: RollingUpdate` with `maxUnavailable: 0`** — capacity never
  dips below desired during a rollout.
- **`terminationGracePeriodSeconds: 30`** with a `preStop` sleep of a few seconds: the sleep
  covers the Endpoints-propagation race so the LB stops sending new requests *before* the
  process starts draining. This is the single most-asked rolling-update detail.
- **Resource `requests` and `limits` on every container.** Requests are what the scheduler
  packs against; limits are what the kubelet enforces. **HPA on CPU is meaningless without
  requests** — utilization is computed as a percentage *of the request* — so this is a
  prerequisite for §7, not a nicety.
- **`podAntiAffinity` (preferred, by hostname)** so the 3 cloud replicas spread across the 3
  agent nodes. Preferred, not required, so the deployment still schedules on a one-node cluster.
- **PodDisruptionBudget `minAvailable: 2`** — makes `kubectl drain` respect availability instead
  of evicting everything at once. This is what turns "I ran a rollout" into "I reasoned about
  voluntary disruption".
- **Secrets**: `infra/certs/*.pem` and the `.env` values become a `Secret` created imperatively
  from files at cluster-bootstrap time (`kubectl create secret generic --from-file`), driven by
  a Make target. **No PEM or password ever enters a committed manifest** — the existing gitleaks
  gate stays authoritative, and the overlay references the Secret by name only.
- **`terminationGracePeriodSeconds` must exceed `preStop` sleep + the §4.1 `Shutdown` timeout.**
  If it does not, the kubelet SIGKILLs the pod mid-drain and every guarantee above is theatre.
  Write the arithmetic down in the manifest comment (e.g. 5 s preStop + 20 s Shutdown < 30 s
  grace). The cloud image is `alpine:3.19`, so a `preStop` `exec` `sleep` works; a distroless
  base would need an `httpGet` preStop or a Go-side delay instead.
- **Postgres schema**: `services/cloud/db/schema.sql` mounts as a ConfigMap into
  `/docker-entrypoint-initdb.d/`, preserving the exact first-init behaviour the Compose file
  documents. (ConfigMap size limit is ~1 MiB; the schema is far under it.) That behaviour
  includes the sharp edge: the directory runs **only on first init of an empty PGDATA**, so
  editing the ConfigMap does nothing to an existing PVC. Schema changes mean wiping the volume —
  guard that the way `scripts/deploy/smoke.sh` already guards it (`ALLOW_DESTRUCTIVE=1`).
- **Data-tier images stay pinned exactly as Compose pins them.** In particular MinIO must remain
  the `-cpuv1` tag: the default RHEL9-based image dies with `Fatal glibc error: CPU does not
  support x86-64-v2` on the Proxmox target's 2011 CPU, and `cloud-server` depends on MinIO, so
  that one crash takes the stack down. A manifest that reaches for `minio/minio:latest`
  reintroduces a documented outage (see the comment in `docker-compose.yml`).
- **Data tier pinned to the server node** via `nodeSelector`, for the `local-path` reason in
  §2.1 — otherwise the `kubectl drain` demo in §8 can strand Postgres in `Pending` with its PV
  bound to the node you just drained.
- **`REDIS_PASSWORD` stays not-wired-up.** The Goal T12 audit found it documented but connected
  to nothing, and deliberately left it visible rather than silently fixed; turning it on is an
  owner decision logged in `docs/study/00-interview-pending-questions.md`. Carry the same state
  into the manifests — do not quietly "fix" it here.
- **Two trust domains, two Secrets.** The internal mTLS material (`ca-cert.pem`,
  `cloud-server-{cert,key}.pem`) plus the JWT keypair go in the cloud-server Secret; the
  browser-facing edge cert (`infra/traefik/edge-{cert,key}.pem`, SANs `automail.local`,
  `api.automail.local`, `blob.automail.local`) is a *separate* trust domain and belongs in a
  `kubernetes.io/tls` Secret referenced by the IngressRoute. Merging them undoes the separation
  c8716b1 deliberately created.

**Exercised in K6 (`make k8s-failure` → `scripts/k8s/failure-check.sh` + `e2e/k8s_failure_test.go`;
every number in `infra/k8s/RESULTS.md`).** The bullets above stopped being design intentions:
`maxUnavailable: 0` + readiness + the preStop sleep were measured through a full
`rollout restart` under continuous traffic with **zero** non-2xx and zero transport errors; the
PDB allowed one eviction and the API server **refused** the second with `TooManyRequests`; the
*preferred* anti-affinity let a `kubectl drain` reschedule the evicted replica instead of
turning the drain into an outage; and an entry orphaned in a deleted pod's PEL came back via
`XAUTOCLAIM` on a survivor — the first end-to-end demonstration of the crash-recovery path in
the assembled product, and the concrete thing K6 has that T9 could not.

Four traps that only appear once you try to *measure* any of this, all of which cost a run:

- **A terminating pod keeps `status.phase: Running`** for its whole grace period, so
  `--field-selector=status.phase=Running` still lists the pods a rollout has already replaced.
  "Did the tier actually turn over?" has to be answered from `metadata.deletionTimestamp`, which
  no field selector exposes — fetch JSON and filter client-side.
- **`kubectl rollout status` returns before the outgoing pods are gone.** Any assertion about
  cluster-wide bookkeeping they clean up on exit (here: the `dispatchers` consumer group) must
  *converge*, not sample, or a pod mid-drain reads as a leak.
- **Do not drain the node running Traefik.** It reschedules fine, but it moves the ingress that
  every suite reaches the cluster through, and the next scenario's traffic then opens with a
  burst of EOFs that look exactly like dropped requests. Moving the edge is a real disruption;
  it is just a different one from "can the app tier be replaced under load".
- **`docker pause` on the printer does not disconnect it.** The TCP connection stays open, so the
  cloud pod keeps the socket and stays subscribed to `mailbox:<id>:dispatch` — dispatch goes on
  succeeding into a process that cannot answer. A paused peer is not a disconnected peer, and
  nothing below the application can tell the difference. Pausing is still the right instrument
  for widening a failover window (what removes the subscriber there is the *pod* being deleted);
  a scenario that needs "no live socket" has to stop the container.

---

## 6. The printer boundary — why it is not a workload

The printer is not a service that happens to run on a server; it is **software inside a physical
mailbox unit** (`CLAUDE.md`, `plans/04-printer-microservice.md`). It owns a private key, a
mailbox-scoped mTLS client cert, and a physical CUPS queue. It dials *out* through NAT precisely
so it never needs to be routable. Putting it in a Deployment would invert the architecture.

The `--scale printer=2` measurement makes this concrete rather than theoretical: two replicas
sharing a `MAILBOX_ID` both subscribe to `mailbox:<id>:dispatch`, so a single dispatch is
delivered twice → two decrypts, two prints of the same letter. The printer's replica count is
**structurally 1 per mailbox**; the fleet scales by adding mailboxes, not replicas.

**For the demo:** the printer stays a plain container on the host (or, later, the real Pi),
pointing `CLOUD_SERVER_WS_URL` at a **NodePort** Service exposing the cloud-server mTLS listener
on 8443. This is strictly better than hiding it in-cluster, because it proves the interesting
property: the printer dials in and lands on *one arbitrary pod*, and a job submitted to either
of the other two still reaches it via the Redis fan-in. That is the same assertion
`infra/compose/full.yml` makes today, re-proved on a cluster where the pods are on different
nodes.

**One physical printer is not a limitation for this demo.** The scaling claim is about the
stateless tier; the printer count is a property of the physical fleet, and the fan-in is proven
with one printer plus ≥2 cloud pods.

### 6.1 What "point it at the NodePort" actually requires

Three concrete obstacles sit between the sentence above and a working dial-in. All three are
measurable in the current tree; none is optional.

**The certificate SANs.** `infra/certs/gen.sh` issues the cloud-server cert with
`subjectAltName = DNS:cloud-server, DNS:localhost` — confirmed against the live
`cloud-server-cert.pem`. `services/printer/mtls.go` sets `RootCAs` but **no `ServerName`**, so
the verified name comes straight from `CLOUD_SERVER_WS_URL`'s host. Any dial host other than
`cloud-server` or `localhost` — `host.docker.internal`, a k3d node IP, `k3d-<cluster>-serverlb` —
fails verification. Two acceptable resolutions, and the goal must pick one rather than discover
this at runtime:

- **(a) Dial `wss://localhost:<nodePort>` from the host network.** `DNS:localhost` already
  covers it, the PKI is untouched, and k3d maps the NodePort to `127.0.0.1` — *provided the
  cluster config declared that port mapping at creation time* (see below).
- **(b) Add SANs to `gen.sh` and regenerate the internal PKI.** Correct but expensive: it
  invalidates the certs every Compose consumer is holding, so it forces a full stack restart and
  a re-run of `make deploy-smoke` to prove the Compose path still works.

**The printer is a container, so `localhost` is the wrong loopback.** Inside a Compose container
`localhost` is the container itself, not the host, so option (a) additionally needs
`network_mode: host` on the printer (or running the printer as a bare host process). And the
base Compose printer carries `depends_on: cloud-server`, so `docker compose up printer` drags
the entire Compose stack up alongside the cluster — a printer-only override file is required.

**Port mappings are fixed at cluster-creation time.** k3d publishes nothing to the host by
default beyond the API server. Both the ingress ports (§8.1) and this mTLS NodePort must be
declared in `infra/k8s/k3d-cluster.yaml` up front; retrofitting a port means destroying and
recreating the cluster.

**A fourth obstacle, found on the first K5 run and not anticipated above: object storage.**
The dial-in is only half the printer's network use — it then fetches the ciphertext from the
pre-signed GET URL in the dispatch frame. `dispatch/route.go` signs that URL with
cloud-server's **internal** MinIO client, so its host is `minio:9000`, and SigV4 signs the
`Host` header *including the port* — the request cannot be redirected to another address
without invalidating the signature. Whatever the printer dials must therefore answer to the
literal name `minio` on port 9000. A NodePort cannot (range 30000–32767, and the port is in
the signature); a k3d host mapping cannot (frozen at creation, per the paragraph above);
`kubectl port-forward` cannot, because `docker-compose.{e2e,full}.yml` publish the *Compose*
MinIO on `0.0.0.0:9000` and the forward would either fail to bind or silently aim the printer
at the wrong object store. What works is a **`hostPort: 9000` on the MinIO pod** in the k3d
overlay: it binds inside the k3d *node* container, whose IP is routable from the host because
k3d nodes are Docker containers on a bridge, and the printer maps the name onto that IP with
`extra_hosts`. Measured: `172.19.0.2:9000` answers `/minio/health/live` from the host. It is
overlay-only, and it is safe only because the data tier is already pinned to one node (§5) —
a `hostPort` is a per-node exclusive resource and would otherwise cap replicas at one per
node. `scripts/k8s/e2e.sh` resolves the node IP at run time (`kubectl get pod minio-0
-o jsonpath='{.status.hostIP}'`); Docker assigns it, so it must never be hardcoded.

### 6.2 Proving fan-in when you cannot address a pod

The Compose full-system suite proves fan-in by giving the two cloud replicas **distinct names
and distinct host ports** (`infra/compose/full.yml`), precisely because `--scale` replicas share
one alias and cannot be individually addressed. A Kubernetes Service load-balances, so that
lever is gone: "submit to a pod that is *not* the socket owner" needs a stated method or the
acceptance is not executable. Pick one:

- `kubectl port-forward pod/<name> 8080` to address a specific pod directly, after identifying
  the socket owner from its logs (`printer-link: mailbox <id>`); or
- submit through the Service and read the `X-Automail-Node` response header, retrying until it
  differs from the owner's pod name.

Seeding has the same shape of problem: `scripts/e2e/seed.sh` execs `psql` through
`docker compose exec postgres` (parameterised only over *which Compose files*). A cluster run
needs a `kubectl exec` backend for it, reading the same `.env` values that populate the K2
Secret.

**Resolved in K5 (`scripts/k8s/e2e.sh`): the first option, plus a step the list above omits —
the owner must be *discovered* before a non-owner can be chosen.** Under Compose the owner is
known by construction (the printer dials a fixed alias); here kube-proxy picks. So the script
timestamps the printer container's creation, waits for `mailbox:<id>:state` to read `idle`
(the hub seeds that key only after acking the register frame, making it the reliable
readiness signal — the socket existing is not), then greps every cloud-server pod's logs with
`--since-time` for `printer-link: mailbox <id> registered`, newest line winning so a mid-run
reconnect resolves correctly. It port-forwards to any other pod, and `e2e/k8s_test.go`
re-checks the `X-Automail-Node` header before trusting the forward — a forward accidentally
aimed at the owner would make the fan-in assertion pass for the wrong reason. Observed across
two consecutive runs: the socket landed on two different pods, which is the property, not a
flake.

---

## 7. The autoscaling demo (the part with a number on it)

1. Enable `metrics-server` (k3d ships it; k3s has it by default).
2. `HorizontalPodAutoscaler` on cloud-server: `minReplicas: 2`, `maxReplicas: 8`, target 60 %
   CPU utilization.
3. Drive it with the **existing** k6 submission script (`scripts/load/submission.js`) — no new
   load tooling, and the baseline in `scripts/load/baseline.json` is already calibrated on this
   host, so the before/after is comparable.
4. Record: pod count over time, p95 submission latency at each replica count, and the scale-down
   after load stops (note the default 5-minute stabilization window — explaining *why* scale-down
   is deliberately slower than scale-up is a good interview beat).

**Provenance discipline applies** (`notes/resume-cheatsheet.md`): every number is from a k6 run on
a WSL2 dev host, said out loud, unprompted. "Autoscaled 2→N pods under k6 load on my dev host"
is defensible; "handles N rps in production" is not.

**Done in Goal K7** — `make k8s-load` (`scripts/k8s/load-check.sh` → `scripts/load/k8s-report.py`),
measurements in `infra/k8s/RESULTS.md`. Measured: **2 → 7 replicas** under three waves of load
(`maxReplicas: 8` deliberately not reached — the controller sized to the load), peak utilization
**124 %** of the 60 % target, worst p95 **10.79 ms at one replica → 5.80 ms autoscaled** under the
identical offered load, 0 % errors throughout, and scale-down beginning **283 s** after the load
stopped. The run is a real gate, not a report: it asserts that the tier scaled past `minReplicas`,
that the autoscaled p95 is **no worse than its own single-replica control**, that errors stayed
under 5 %, and that scale-down actually happened.

### 7.1 What "drive it with the existing k6 script" costs

- **k6 is not installed on this host.** Every existing load run uses the pinned k6 *container* on
  the Compose network (`infra/compose/load.yml`). The cluster equivalent is a k6 Job (or
  `kubectl run --rm`) **inside** the cluster — not a host-side run through the ingress, which
  would add TLS, Traefik and the guest rate limit to the measured path and invalidate the
  comparison outright.
  *Measured (K7): a Job per wave, `completions == parallelism`, template at
  `infra/k8s/load/k6-job.yaml`. Two costs the plan did not name. **(a) The k6 image cannot be
  imported.** `k3d image import grafana/k6:2.1.0` fails with `ctr: content digest … not found`
  (a registry-pulled multi-platform image) and **still exits 0**, leaving the cluster with no
  such image and the failure surfacing later as `ErrImageNeverPull`; a one-line
  `FROM grafana/k6:2.1.0` rebuild under a local tag is single-platform and imports cleanly, and
  the script verifies with `crictl` per node rather than trusting the exit code. **(b) A Job pod
  has no shared filesystem**, so `handleSummary`'s `/report/submission.json` cannot be
  bind-mounted out: the pod command prints the JSON between markers and the run parses it from
  `kubectl logs`. The emptyDir it writes to needs `fsGroup: 12345`, or the k6 user cannot write
  and k6 exits 0 regardless — the same silent-summary-loss trap `infra/compose/load.yml`
  documents for the uid mapping.*
- **The load profile overrides `MINIO_PUBLIC_ENDPOINT` to `""`.** `submission.js` follows the
  pre-signed `upload_url`, and the base config signs it for `blob.automail.local`, which nothing
  in-cluster can resolve. The k8s run needs the same override (a Kustomize `load` patch), or
  every submission fails at the PUT.
  *Done (K7): `infra/k8s/overlays/k3d-load`, an overlay **of** `k3d-local` whose only content is
  a `configMapGenerator` with `behavior: merge` overriding that one literal. Merging rather than
  patching the Deployment matters — the value reaches the pod through a hashed ConfigMap name, so
  changing it rolls the pods onto the new value instead of leaving them running the old one. The
  run reverts to `k3d-local` on exit **including on abort**, because a cluster left signing
  `minio:9000` breaks the browser guest flow with no error anywhere near the cause.*
- **The committed baseline is not an apples-to-apples gate.** `scripts/load/baseline.json` was
  measured against a **single** cloud replica with **no CPU limit**. Under Kubernetes with
  requests/limits and ≥2 replicas the topology is different, so treat the Compose baseline as
  context, not as pass/fail. The honest move is to record a fresh single-replica reference on the
  cluster first, then report the HPA run against *that* — and say both numbers came from a WSL2
  dev host.
  *Done (K7): the reference is phase one of the same run — HPA deleted, `replicas: 1`, identical
  waves — and it is what the acceptance gate compares against. The Compose baseline is quoted as
  context in `RESULTS.md` with the topology difference spelled out, never as pass/fail. A useful
  accident of `ramping-arrival-rate`: its iteration count is deterministic, so every generator pod
  in every phase ran exactly the **1812** iterations `baseline.json` recorded, which makes the
  per-generator workload literally identical across the two deploy targets.*
- **HPA target is a percentage of the CPU `request`, so §5's request sizing decides whether this
  demo works at all.** Too large a request and k6 never crosses 60 %; too small and it pins at
  `maxReplicas` immediately. Cross-check the chosen request against `maxReplicas: 8` × request
  fitting inside the ~10 GiB / 24-core budget from §2.1, or the demo ends in `Pending` pods.
  *Measured (K7): the K3 sizing held. One k6 pod drives ~110 m of cloud-server CPU here, i.e. just
  over one 100 m request — not enough to cross 60 % at two replicas — so the load is multiplied by
  running the **same** script in three pods rather than by editing its stages, which is what keeps
  the Compose numbers comparable. Three generators took the tier to 124 % of target and 7 replicas,
  short of `maxReplicas: 8`: sized to the load, no `Pending` pods.*
- **metrics-server needs to be healthy, not merely present.** k3s ships it; in k3d it sometimes
  needs `--kubelet-insecure-tls`. The HPA also needs ~15–30 s of metrics before it reacts, so a
  short k6 stage will show nothing.
  *Measured (K7): healthy out of the box on this k3d cluster — no `--kubelet-insecure-tls` needed.
  The reaction lag is real and is why waves exist: the HPA first reported a usable reading 16 s
  after being created, and during wave one it read 25 % → 67 % → 124 % of target across ~35 s
  before acting. A single 65 s wave would have ended before the controller moved.*
- **Scale-down uses a 300 s stabilization window by default.** Either budget the run long enough
  to observe it or set `behavior.scaleDown.stabilizationWindowSeconds` explicitly — and say which
  you did, since "why is scale-down deliberately slower than scale-up" is the interview beat.
  *Done (K7): **both** — the value is written out explicitly in `hpa.yaml` (at the 300 s default,
  so the manifest states a decision instead of inheriting one) *and* the run waits it out rather
  than shortening it for convenience. Measured: first scale-down 283 s after the load stopped,
  `minReplicas` 299 s after. Slightly under 300 s because the window is anchored to the last
  elevated **recommendation** the controller stamped — which it computed shortly before the final
  requests drained — not to the moment traffic stopped.*

---

## 8. What this demo does *not* prove — say these first

Volunteering the limits is worth more than the demo itself.

- **The data tier is three single points of failure.** One Postgres, one Redis, one MinIO, each
  with one PVC. A StatefulSet with `replicas: 1` gives stable identity and storage, **not**
  availability. This is the real scaling frontier and it is already written up as V3
  (`plans/15-v3-roadmap.md`) — knowing where the limit is scores as much as the scaling does.
- **One physical host.** Four k3d nodes are four containers on one kernel. Node anti-affinity
  and drain behaviour are genuinely exercised; correlated hardware failure is not.
- **Redis pub/sub is at-most-once and load-bearing for dispatch.** Kubernetes does not change
  this. The durability answer is the Streams PEL + `XAUTOCLAIM`, not the orchestrator.
- **Self-signed certs, no cert-manager.** The internal CA is still `infra/certs/gen.sh`.
  cert-manager is a natural follow-on (§9) but is not required for the scaling claim.
- **No service mesh, no network policy.** Pod-to-pod traffic inside the cluster is unrestricted;
  the mTLS invariant is enforced at the application layer, as it is today. A `NetworkPolicy`
  denying everything except the known edges is the honest next step.

### 8.1 The non-standard-port cascade (browser access on this host)

Windows holds 80 and 443 on this machine, so the k3d loadbalancer publishes the edge on
something like `:8443`. Four committed values silently assume port 443, and all four break
together — with no server-side error, which is exactly the class of bug the T12 deploy audit
kept finding:

| Value | Where | Why a non-443 port breaks it |
|---|---|---|
| `connect-src 'self' https://blob.automail.local` | `infra/traefik/dynamic.yml` (`secure-headers`) | A CSP host-source with no port means *the scheme's default port*. On `:8443` the browser blocks the guest's ciphertext PUT. |
| `MINIO_CORS_ORIGIN=https://automail.local` | `.env` | The preflight's `Origin` carries the port, so it no longer matches and the PUT is refused. |
| `MINIO_PUBLIC_ENDPOINT=blob.automail.local` | `.env` → pre-signed URL | MinIO signs the `Host` header; a non-default port must be part of the endpoint or the signature mismatches. |
| `/etc/hosts` | host | Absent entirely — no browser can resolve any of the three names today. |

So K4-style browser acceptance has a real fork in it, and it should be chosen deliberately rather
than discovered: **either** free 80/443 on the host (owner action) and keep every value as-is,
**or** parameterise all four in the k3d overlay. Note that no existing suite would have caught
this: T7/T8 bypass Traefik entirely, and the T12 smoke drives the edge with `curl --resolve` plus
a Go transport, never a browser.

Also carry `sniStrict: true` across as a Traefik `TLSOption` CRD. It is not decoration — it is
the regression guard for c8716b1, and dropping it during the port makes the
`ERR_SSL_UNRECOGNIZED_NAME_ALERT` class of first-deploy bug silently possible again.

Two smaller porting notes for the ingress:

- **Rate-limit placement is a T12 finding, not a detail.** The browser never contacts
  `api.automail.local`; it calls same-origin `/api/*`, which Next proxies server-side. The limit
  therefore has to sit on the **portal** router (`portal-guest`), with `burst` matched to
  `average` (Traefik defaults burst to 1, which turns "20/min" into one request per 3 s and 429s
  the guest flow on its second call). Both properties must survive the port.
- **Check what the rate limit keys on.** Behind the k3d loadbalancer container, client source
  IPs may collapse to the LB's address, turning a per-IP limit into a global one. The acceptance
  "the rate limit still throttles" could pass for the wrong reason. Measure the `sourceCriterion`
  behaviour rather than assuming it carried over.

---

## 9. Optional follow-ons (only after the demo is green)

| Add | Buys | Cost |
|---|---|---|
| Helm chart (converted from the Kustomize base) | The literal keyword; templated multi-env values | Low — mechanical conversion |
| `cert-manager` + internal ClusterIssuer | Automated cert rotation; drops `gen.sh` from the deploy path | Medium |
| `NetworkPolicy` default-deny | A real defense-in-depth story next to the mTLS invariant | Low |
| Prometheus + ServiceMonitor | Replaces `X-Automail-Node` header spelunking with actual per-pod metrics | Medium |
| k3s on the Proxmox host | Same manifests, real hardware, off the dev laptop | Medium |

---

## 10. Resume bullets this earns (and the file that defends each)

Draft — final wording goes in `notes/resume-cheatsheet.md` alongside the existing sections, with
its own follow-up Q&A block.

- *"Orchestrated the stateless tier on Kubernetes (k3d/k3s, 4 nodes): rolling updates with zero
  dropped requests, pod anti-affinity, and HPA autoscaling 2→8 replicas under k6 load."*
  → `infra/k8s/base/cloud-server/`, the HPA run record, `scripts/load/submission.js`.
  **Superseded (K8): `2→8` became `2→7`** in the final wording — 8 is `maxReplicas`, 7 is what the
  run measured, and quoting the ceiling would be claiming a number `RESULTS.md` does not contain.
  The draft phrasing is retired in `notes/resume-cheatsheet.md` Appendix B rather than deleted.
- *"Kept cross-node correctness under orchestration: dispatch fan-in and status fan-out over
  Redis pub/sub, with Streams consumer-group cleanup on pod termination."*
  → `services/cloud/dispatch/`, `services/cloud/link/hub.go`, the DELCONSUMER shutdown path.

**Where the numbers live.** "Every number traceable to a recorded run" needs somewhere committed
to trace *to*. The failure and rollout scenarios (§5, §8) and the HPA run (§7) must each write
their raw output into a tracked file — `infra/k8s/RESULTS.md` — at the time they are run. Without
that, the cheat-sheet section is written from memory, which is the one thing
`notes/resume-cheatsheet.md` exists to prevent.

**The trap to rehearse:** *"So Kubernetes made it scale?"* — No. The tier was stateless and
coordinated through Redis before any of this; Kubernetes schedules it, restarts it, and reacts
to load. Then name the one thing k8s enabled that Compose could not: surviving a rolling update
with no dropped requests.

**Done in Goal K8:** `notes/resume-cheatsheet.md` §9 (bullet, defending files, 30-second answer,
eleven follow-ups led by the trap above) and `docs/study/28-kubernetes-orchestration.md`
(Deployment vs StatefulSet, readiness vs liveness, the termination sequence, HPA mechanics, and
the §8 limits written as things to volunteer). Every figure in §9 is quoted from a line in
`infra/k8s/RESULTS.md`.
