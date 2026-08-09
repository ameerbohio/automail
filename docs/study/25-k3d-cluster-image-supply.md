# k3d: the local cluster and how images get into it

*Code: `infra/k8s/k3d-cluster.yaml`, `scripts/k8s/{tools,up,images,down,validate}.sh`, `scripts/k8s/versions.env`. Plan: `plans/16-kubernetes.md` §2. Host notes: `docs/k8s-host-setup.md`. Acceptance: `make k8s-up && make k8s-images && make k8s-down`.*

## What it is

k3d runs **k3s** (a single-binary, CNCF-conformant Kubernetes distribution) with each node as a Docker container on the daemon this project already uses. `make k8s-up` produces four nodes — one server (control plane + etcd-replacement SQLite) and three agents — plus a small nginx-ish `serverlb` container that fronts them.

Four nodes rather than one is the whole point: pod anti-affinity, `kubectl drain`, "which node did that replica land on" and rescheduling are only demonstrable with somewhere else to go.

## Why k3d over kind or minikube

| Option | Why not / why yes |
|---|---|
| **k3d** ✅ | k3s **bundles Traefik** as its ingress controller. This project's routing is already Traefik, so the Compose labels become `IngressRoute`/`Middleware` CRDs almost line for line — including `secure-headers` and `guest-ratelimit`. Multi-node is a flag. `k3d image import` removes the need for a registry. |
| kind | No bundled ingress (install one, learn its annotation dialect) and clumsier image loading. |
| minikube | A VM per cluster: heavier, slower, and another virtualisation layer under WSL2. |

The escape hatch matters too: on the Proxmox host this becomes **k3s proper** — the same control plane without the Docker wrapper — and the manifests carry over unchanged.

## The two things that are genuinely different from Compose

### 1. Ports are frozen at creation time

k3d publishes host ports by wiring them into the node/loadbalancer **containers**, and Docker cannot add a published port to a running container. So a port you forgot means `k3d cluster delete` + recreate — taking every PVC with it. Every mapping this track will ever need is therefore declared up front in `k3d-cluster.yaml`: the edge (9080/9443), the printer's mTLS NodePort (`127.0.0.1:9843 → 30843`) and the API server (6445).

The edge is 9080/9443 rather than the obvious 8080/8443 because **the Compose stack owns those**. Distinct ports let both stacks run at once, which is what makes "Kubernetes is an additional target, not a replacement" a testable claim rather than a slogan.

### 2. There is no registry — and that has a sharp edge

`k3d image import` copies a locally built image into every node's containerd. `docker build` stays the only build path; no push, no credentials. Two traps come with it:

- **`:latest` is poison here.** Kubernetes forces `imagePullPolicy: Always` when the tag is `latest`, and a cluster with no registry answers that with `ImagePullBackOff`. Hence explicit `:dev` tags plus `imagePullPolicy: IfNotPresent`. `make k8s-validate` fails the build if any rendered manifest reintroduces `:latest`.
- **Import keys on image *ID*, not tag.** Rebuild under the same tag and the ID changes, but nodes keep the old layers and pods keep running the old binary — a debugging session that ends in confusion. `scripts/k8s/images.sh` therefore re-imports **and** `kubectl rollout restart`s any Deployment already on that tag.

Verification is done from the cluster's side (`crictl images` on each node container), not Docker's: containerd is what kubelet pulls from, so that is the thing the import had to reach. The stronger proof is a pod started with `IfNotPresent` whose event reads *"Container image … already present on machine"* — nothing was fetched from a registry.

## The honest caveat (the follow-up an interviewer asks)

> *"Four nodes — so you tested a real distributed failure?"*

No. **Four k3d nodes are four containers sharing one kernel, one page cache and one disk.** What is genuinely exercised: the scheduler, Services/kube-proxy, rolling updates, anti-affinity, PDBs, HPA behaviour, and the pod lifecycle (which is what surfaced the graceful-shutdown bug — see [24-graceful-shutdown-consumer-lifecycle.md](24-graceful-shutdown-consumer-lifecycle.md)). What is *not*: network partitions, node-level hardware failure, kernel isolation, or real etcd quorum — k3s here uses embedded SQLite, so there is one control plane and no HA story at all. `plans/16-kubernetes.md` §8 keeps that list; leading with it is more persuasive than being caught by it.

A second caveat with a shelf life: this WSL2 kernel is **cgroup v1**, which Kubernetes has had in maintenance mode since 1.31. It works on the pinned k3s 1.33 — measured, all four nodes `Ready` — and that is exactly why the pin exists and why raising it is a re-verification, not a bump.
