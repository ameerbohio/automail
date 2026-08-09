# The device that is not a workload: dialing into a cluster from outside it

*Code: `docker-compose.k8s-printer.yml`, `scripts/k8s/e2e.sh`, `e2e/k8s_test.go`, `infra/k8s/overlays/k3d-local/kustomization.yaml`. Plan: `plans/16-kubernetes.md` §6. Acceptance: `make k8s-e2e`.*

## What it is

Everything stateless moved into Kubernetes — cloud-server, portal, the data tier. The **printer stayed out**, running as a plain container on the host, dialing *out* to the cluster's mTLS listener over a NodePort. This note is about why that is the correct architecture rather than an unfinished migration, and about the three concrete obstacles between "point it at the NodePort" and a job actually printing.

## Why the printer must not be a Deployment

The printer is not a service that happens to run on a server. It is software inside a **physical mailbox unit**: it owns an RSA private key, a mailbox-scoped mTLS client certificate, and a CUPS queue attached to actual paper. It dials out through NAT precisely so it never has to be routable — no inbound firewall hole at every install site.

The decisive argument is not architectural taste, it is a measurement. Two replicas sharing a `MAILBOX_ID` both subscribe to `mailbox:<id>:dispatch`, so a single dispatch is delivered to both: two decrypts, two prints, **one letter printed twice**. The printer's replica count is structurally 1 per mailbox. The fleet scales by adding mailboxes, not replicas.

So `replicas:` — the field a Deployment exists to give you — is meaningless here, and `kubectl scale` on it would be a correctness bug. That is the test for whether something belongs in a Deployment: *would a second copy be an improvement or a defect?*

**The interview version:** "The stateless tier is in Kubernetes and the device isn't, because the device is a device. Putting it in a Deployment would invert the architecture — an outbound-only unit behind NAT would have to become routable, and its replica count is pinned at one by physics, not by capacity."

## Obstacle 1 — the certificate decides the hostname

`services/printer/mtls.go` sets `RootCAs` but **no `ServerName`**, so Go verifies the server certificate against whatever host is in `CLOUD_SERVER_WS_URL`. The cloud-server cert (`infra/certs/gen.sh`) carries `subjectAltName = DNS:cloud-server, DNS:localhost` and nothing else.

That single fact eliminates most of the obvious dial addresses: `host.docker.internal`, a k3d node IP, the k3d serverlb container name — all fail verification before a byte of WebSocket is exchanged. Two options remain, and the cheap one wins: **dial `wss://localhost:9843`** and let k3d publish the NodePort on `127.0.0.1`. Reissuing the internal PKI with new SANs would have invalidated every certificate the Compose stack's consumers hold and forced a full re-proof of that path, to buy a name that is no better.

The consequence propagates: inside a bridged container `localhost` is the container itself, so the printer must run with `network_mode: host` for that name to mean the host's loopback. And k3d fixes host port mappings at **cluster creation** — `127.0.0.1:9843 → nodePort 30843` had to be declared in `infra/k8s/k3d-cluster.yaml` before anything depended on it, because retrofitting a port means destroying the cluster and every PVC with it.

## Obstacle 2 — SigV4 signs the hostname too

This one is not in the plan; it surfaced on the first run.

The dispatch frame carries a **pre-signed GET URL** for the ciphertext blob, and cloud-server signs it with its *internal* MinIO client — so the URL's host is `minio:9000`, a name that only resolves inside the cluster. AWS SigV4 signs the `Host` header, port included, so this cannot be fixed by redirecting the request: change the host and the signature fails.

Whatever the printer dials must therefore **answer to the literal name `minio` on port 9000**. Three ways to give an outside process that, and only one works here:

| Mechanism | Why it fails / works |
|---|---|
| NodePort | Range is 30000–32767. The port is baked into the signature. Dead. |
| k3d host port mapping | Frozen at cluster creation; retrofitting destroys the PVCs. Dead. |
| `kubectl port-forward` on the host | Host `:9000` is not ours — `docker-compose.e2e.yml` publishes the *Compose* MinIO on `0.0.0.0:9000`. It would either fail to bind or, worse, silently point the printer at the wrong object store. Dead. |
| **`hostPort` on the MinIO pod** | Binds inside the k3d **node container**, whose IP is routable from the host because k3d nodes are Docker containers on a bridge. Works. |

So the overlay gives MinIO `hostPort: 9000`, and the printer maps the name onto the node's IP with a Docker `extra_hosts` entry. The `Host` header stays `minio:9000` and the signature verifies untouched.

Two things make that safe rather than a hack. A `hostPort` is a per-node exclusive resource, so it silently caps a workload at one replica per node — harmless here **only because** the data tier is already pinned to a single node by the `local-path` storage decision. And it lives in the k3d overlay, not the base: it is a fact about reaching this host's cluster, exactly the kind of thing a `proxmox` overlay would drop.

The general lesson is worth keeping: **a pre-signed URL is a capability bound to a hostname.** Anything that rewrites the request path — a proxy, a NodePort, a different DNS name — invalidates it. That is the same constraint that forced `MINIO_PUBLIC_ENDPOINT` to exist for the browser's upload, seen from the other side.

## Obstacle 3 — you cannot address a pod through a Service

Compose proves cross-node fan-in by giving the two cloud replicas distinct names and distinct host ports, so the driver can deterministically submit to "the node that does **not** hold the printer socket". A ClusterIP Service load-balances; that lever is gone, and "submit and hope kube-proxy picks the other one" is not a test.

Worse, in Compose the owner is known *by construction* — the printer dials a fixed alias. Here kube-proxy picks a pod, so the owner has to be **discovered**:

1. Note the time, start a fresh printer container, wait for `mailbox:<id>:state` in Redis to read `idle` (the hub seeds that key only *after* acking the register frame, which is why it is a reliable readiness signal and the socket's existence is not).
2. `kubectl logs --since-time=<start> --timestamps` every cloud-server pod and grep for `printer-link: mailbox <id> registered`. Newest line wins, so a mid-run reconnect resolves correctly.
3. `kubectl port-forward` to any *other* pod, and have the driver re-check the `X-Automail-Node` response header before trusting the forward — a guard for the guard, since a forward accidentally aimed at the owner would make the assertion pass for the wrong reason.

Across two consecutive runs the socket landed on two different pods, which is the property being described rather than a flake.

### The assertion itself is one field

`POST /jobs` publishes to `mailbox:<id>:dispatch` and counts subscribers. With zero it gives up and leaves the job `queued` for the Redis Streams retry path. So a **`dispatching`** response from a pod that holds no socket can only mean the owner pod — on another node — received that publish over Redis and relayed it down its own connection.

No sleeps, no polling, no timing assumptions: one field in one response proves the cross-pod hop. Reading the SSE stream from that same non-owner pod up to `delivered` proves the return path, since that status originated on the owner's socket and crossed Redis in the other direction.

## What this actually proves, and what it does not

- **Proved:** the printer reaches `delivered` from outside the cluster; mTLS survives kube-proxy (TLS terminates in the pod, so the NodePort is a dumb L4 hop); the RAM-only invariant holds across the boundary — `/dev/shm` in the printer container is empty after delivery, checked with `docker exec` because the printer is not a pod; cross-pod fan-in and fan-out over Redis.
- **Not proved:** anything about real network partitions. Four "nodes" are four containers on one kernel, the printer is a container on that same kernel, and its "outside the cluster" is a Docker network boundary rather than the internet and a NAT. The *code paths* are the production ones; the *failure modes* of a real WAN are not exercised. Say this before being asked.

## Interview one-liners

- *"Why isn't the printer in the cluster?"* — Because a second replica would print the letter twice. Replica count is a property of the physical fleet, not of load.
- *"How does an outbound-only device work with Kubernetes?"* — It dials a NodePort with a client certificate; the cluster never initiates. Kube-proxy load-balances the dial, so which pod owns the socket is arbitrary, and Redis pub/sub makes that irrelevant to the other pods.
- *"What broke first?"* — Object storage. The pre-signed URL is signed against the in-cluster hostname, and SigV4 covers the `Host` header, so the printer had to be given that exact name-and-port from outside. A `hostPort` on the pod was the only mechanism that fit.
