# Kubernetes Host Setup — Local k3d Cluster (Goal K1)

**Status: DONE — a 4-node k3d cluster boots on this WSL2 host and both service
images run in it (verified 2026-08-08).**

This is the host-side companion to [plans/16-kubernetes.md](../plans/16-kubernetes.md)
§2: what the Kubernetes track needs from the machine, what it installs itself,
and — the part worth reading — which of the plan's predicted blockers turned out
to be real.

The cluster is an **additional** deployment target. `docker-compose.yml`,
`make deploy-smoke`, `make test-e2e-full` and the demo scripts keep working
exactly as before; the two can even run at the same time (see *Ports* below).

---

## Tooling (installed by `make k8s-tools`, no sudo)

| Tool | Pinned version | Installed to |
|---|---|---|
| `k3d` | `v5.9.0` | `~/.local/bin/k3d` |
| `kubectl` | `v1.33.13` | `~/.local/bin/kubectl` |
| k3s node image | `rancher/k3s:v1.33.13-k3s2` | pulled by k3d at cluster creation |

Both are single static binaries, so nothing needs root — which matters here:
this host has no passwordless sudo, and a setup step that silently prompts for
a password is a setup step that hangs. Both downloads are **checksum-verified**
(`scripts/k8s/tools.sh`); these binaries receive cluster credentials, so
"download and chmod +x" would be the one un-audited supply-chain step in a repo
that otherwise runs gitleaks, gosec, govulncheck and osv-scanner.

Versions live in one place, [`scripts/k8s/versions.env`](../scripts/k8s/versions.env).
`kubectl` is deliberately held to the **same minor** as k3s (1.33): inside the
±1 skew policy, and one fewer variable when something behaves oddly.

Why these pins, rather than "latest":

- **k3s 1.33, not 1.35/1.36.** Kubernetes put cgroup v1 into maintenance mode in
  1.31, and this kernel is cgroup v1 (below). Choosing the newest minor would be
  betting the whole track on the least-tested combination.
- **The bundled Traefik version is a function of the k3s tag** — k3s
  v1.33.13-k3s2 ships **Traefik 3.7.8** with the `traefik.io/v1alpha1` CRDs
  (`IngressRoute`, `Middleware`, `TLSOption`, …). Goal K4 ports the Compose
  Traefik configuration onto exactly those CRDs, so a floating tag would move
  the ingress dialect underneath it.

## Host prerequisites, measured

The plan's preflight (§2.1) flagged three rows that could stop the track dead.
Here is what each actually did:

| Prerequisite | State | Verdict |
|---|---|---|
| Docker Engine | 29.6.1 | Fine — k3d nodes are containers on the daemon Compose already uses. |
| `k3d` / `kubectl` | were absent | Installed, pinned, checksum-verified. |
| **cgroup v2** | **not present — cgroup v1 (hybrid): `/sys/fs/cgroup` is `tmpfs`, v1 controllers alongside a `cgroup2` mount at `/sys/fs/cgroup/unified`; `docker info` reports `Cgroup Version: 1`** | **Not a blocker after all.** k3s v1.33.13 registered all four nodes `Ready`, and CoreDNS, metrics-server, local-path-provisioner, Traefik and klipper-lb all reached `Running`. **No owner action was needed** — the `/etc/wsl.conf` + `wsl --shutdown` remedy the plan reserved stays unused. Treat this as version-bound, not settled: it is the first thing to re-check if the k3s pin is ever raised. |
| Memory | 15 GiB total, ~10 GiB free | Four nodes plus the data tier fit. It still bounds K3's pod `requests` and K7's `maxReplicas: 8`. |
| Host ports 80/443 | held by Windows | The cluster edge uses 9080/9443 instead (below). The CSP/CORS/presign cascade this triggers is K4's problem, documented in §8.1 of the plan. |
| `/etc/hosts` `*.automail.local` | still absent | Only needed for K4's **browser** acceptance (sudo, owner action). `curl --resolve` covers everything before that. |

## Ports (fixed at cluster-creation time)

k3d wires published ports into the node/loadbalancer containers when the
cluster is created, and Docker cannot add a published port to a running
container. **Retrofitting a port means destroying and recreating the cluster**,
taking every PVC with it — so all of them are declared up front in
[`infra/k8s/k3d-cluster.yaml`](../infra/k8s/k3d-cluster.yaml):

| Host | Target | For |
|---|---|---|
| `9080` | Traefik `:80` via the k3d loadbalancer | HTTP edge (K4) |
| `9443` | Traefik `:443` via the k3d loadbalancer | HTTPS edge (K4) |
| `127.0.0.1:9843` | NodePort `30843` on the server node | printer mTLS dial-in (K5) |
| `127.0.0.1:6445` | API server `:6443` | `kubectl` |

The edge is 9080/9443 rather than 8080/8443 for a specific reason: **8080/8443
belong to the Compose stack** (`scripts/deploy/smoke.sh`). Distinct ports mean
the cluster and the Compose stack coexist, which is what keeps Process Rule 3
("the Compose path must keep working") cheap to honour — verified by driving
the Compose edge to a `200` while the cluster was up.

`9843` is bound to `127.0.0.1` on purpose: it is an **internal mTLS** port, not
a public one. It is also why the printer will dial `wss://localhost:9843/...` —
the cloud-server cert's SANs are `DNS:cloud-server, DNS:localhost` and
`services/printer/mtls.go` sets no `ServerName`, so `localhost` is the only
dial host that verifies without regenerating the PKI every Compose consumer
holds (plan §6.1, option (a)).

---

## Usage

```bash
make k8s-tools      # once: install pinned k3d + kubectl into ~/.local/bin
make k8s-up         # create the cluster, wait for 4 Ready nodes  (~26 s)
make k8s-images     # docker build both services, k3d image import  (~17 s)
make k8s-secrets    # namespace + Secrets from infra/certs, infra/traefik and .env
make k8s-apply      # schema ConfigMap + overlay, wait for Ready  (~20 s)
make k8s-data-check # schema applied, and data survives pod deletion
make k8s-validate   # kustomize build + client dry-run — needs no cluster, no Docker
make k8s-down       # delete, then prove no k3d container/network/volume survives
```

`make k8s-validate` is also part of `make ci`, so a manifest typo fails on the
machine that wrote it rather than during a bring-up. It skips cleanly when
`kubectl` is not installed.

### Secrets are never in the repo (Goal K2)

`make k8s-secrets` creates the `automail` namespace and three Secrets from files
that are already gitignored — nothing is copied into the repo and no manifest
contains a credential:

| Secret | Built from | Used by |
|---|---|---|
| `automail-credentials` | `.env` (Postgres, MinIO, `APP_ENCRYPTION_KEY`) | data tier now, cloud-server at K3 |
| `automail-certs` | `infra/certs/` — CA, cloud-server cert/key, JWT keypair | cloud-server at K3 |
| `automail-edge-tls` | `infra/traefik/edge-{cert,key}.pem` | the ingress at K4 |

Internal mTLS and the browser-facing edge cert are **separate trust domains and
separate Secrets** — the split commit c8716b1 created deliberately. Re-running
the target updates the Secrets in place, so rotating a cert is one command.

Prerequisites, and the script fails loudly naming them rather than
half-creating a Secret: `.env` filled in from `.env.example`, plus
`infra/certs/gen.sh`, `gen-jwt-keys.sh` and `gen-edge-certs.sh` having been run.

### The Postgres schema, and when it does *not* apply

`make k8s-apply` creates a `postgres-schema` ConfigMap from
`services/cloud/db/schema.sql` — the same file Compose mounts — and mounts it at
`/docker-entrypoint-initdb.d/`. That directory runs **only on first init of an
empty PGDATA**, exactly as under Compose, so editing the schema does nothing to
an existing PVC. Applying a schema change means destroying the data:

```bash
RESET_DATA=1 ALLOW_DESTRUCTIVE=1 make k8s-apply
```

Both variables are required, the same double opt-in `scripts/deploy/smoke.sh`
uses.

### There is no registry

`k3d image import` copies locally built images into every node's containerd, so
`docker build` stays the only build path. Two consequences that bite:

- **Never tag these `:latest`.** Kubernetes forces `imagePullPolicy: Always` on
  `:latest`, and a cluster with no registry answers that with
  `ImagePullBackOff`. Hence `automail/cloud-server:dev` / `automail/portal:dev`
  plus `imagePullPolicy: IfNotPresent`.
- **Import keys on image ID.** Rebuilding under the same tag changes the ID, and
  the cluster keeps running the old layers until the image is re-imported *and*
  the pods restarted. `scripts/k8s/images.sh` does both — it re-imports and then
  `kubectl rollout restart`s any Deployment already using the tag.

### Troubleshooting

| Symptom | Cause |
|---|---|
| `make k8s-up` fails on a port pre-flight | Something (usually the Compose stack on a non-default `TRAEFIK_*_PORT`) took 9080/9443/9843/6445. |
| Nodes register but never go `Ready` | Suspect the cgroup hierarchy first — re-read the cgroup row above. `k3d node logs k3d-automail-agent-0` shows the kubelet's own complaint. |
| `ImagePullBackOff` on an `automail/*` image | The image was never imported, or a manifest says `:latest`. Run `make k8s-images`; `make k8s-validate` catches the `:latest` case statically. |
| Pods run stale code after a rebuild | Import happened, restart did not. `make k8s-images` covers it; a manual `k3d image import` does not. |
