# StatefulSets, node-local volumes, and keeping key material out of manifests

*Code: `infra/k8s/base/data/*.yaml`, `infra/k8s/overlays/k3d-local/kustomization.yaml`, `scripts/k8s/{secrets,apply,data-check}.sh`. Plan: `plans/16-kubernetes.md` §3, §5. Acceptance: `make k8s-secrets && make k8s-apply && make k8s-data-check`.*

## What it is

The data tier — Postgres, Redis, MinIO — moved from Compose services to **StatefulSets**, one replica each, with headless Services, `local-path` PVCs, and every credential injected from Secrets that are built imperatively at bootstrap and never committed.

## Why a StatefulSet and not a Deployment

The distinction is not "stateless vs stateful" as a vibe; it is three concrete guarantees:

| | Deployment | StatefulSet |
|---|---|---|
| Pod names | random suffix, interchangeable | ordinal and stable (`postgres-0`) |
| Storage | shared or none | one PVC **per ordinal**, from `volumeClaimTemplates`, reattached to the same ordinal |
| Rollout | may run old and new pod simultaneously | ordered, at most one pod per ordinal at a time |

That last row is the one that matters here. A Deployment's rolling update deliberately overlaps old and new pods — for a stateless HTTP server that is availability; for a database over a single volume it is **two postmasters on one data directory**, which is corruption, not redundancy.

The headless Service (`clusterIP: None`) is the other half: no virtual IP, no kube-proxy load balancing, DNS answers with the pod's own address. It is what makes `postgres` and `redis` resolvable under exactly the names `cloud-server` already uses under Compose — `DATABASE_URL` and `REDIS_URL` are byte-identical across both deployment targets, so a bug is never "which environment's connection string".

## The volume is node-local, and that shapes two later goals

k3s's default StorageClass is **`local-path`**: the provisioner makes a directory on a node and hands it over as a PV, with `volumeBindingMode: WaitForFirstConsumer` — the PVC stays `Pending` until a pod is scheduled, so the volume is created *where the pod landed* rather than the pod being dragged to a pre-made volume.

The PV then carries a `nodeAffinity` pinning it to that node forever. Two consequences fall straight out:

- **Delete the pod and the data survives** — because the replacement is scheduled back onto the same node and reattaches the same PVC. `make k8s-data-check` proves it: writes a marker row, `kubectl delete pod postgres-0`, waits for the StatefulSet to recreate it, reads the marker back, and **prints which node it came back on**. Same node, every time, by construction.
- **Drain that node and the pod is `Pending` forever** — nowhere else can satisfy the affinity.

Goal K6 drains a node on purpose. Both facts can only be true at once if the data tier lives somewhere that is never drained, so the k3d overlay pins all three StatefulSets to the **server** node with a `nodeSelector`, leaving the three agents freely drainable. That `nodeSelector` is in the *overlay*, not the base: it is a fact about this cluster's storage, and an overlay with network-attached storage (EBS, Ceph, Longhorn) would delete it outright.

Say the honest version out loud: *data survived because the scheduler put the pod back on the same machine.* That is a storage-class property, not a Kubernetes guarantee, and it is not high availability.

## Secrets: base64 is not encryption

A Kubernetes Secret is base64-encoded, not encrypted. Committing one is committing the credential, in a format that looks reassuring. So none of them is a manifest:

```
make k8s-secrets   →  scripts/k8s/secrets.sh
   .env                  → secret/automail-credentials   (DB, MinIO, app key)
   infra/certs/*.pem     → secret/automail-certs         (internal mTLS CA + JWT keypair)
   infra/traefik/edge-*  → secret/automail-edge-tls      (kubernetes.io/tls)
```

All three sources are already gitignored, so the key material never moves — it goes from the file it lives in straight into the cluster's API. The manifests reference Secrets **by name only**, which means `kubectl kustomize` can never render a credential and `make scan` (gitleaks) stays authoritative without `.gitleaks.toml` being widened for `infra/k8s/`.

Two details worth having ready:

- **Two trust domains, two Secrets.** The internal PKI (the CA that signs cloud-server and printer certs, plus the JWT keypair) and the browser-facing edge cert are unrelated chains that fail independently. They were deliberately separated in commit c8716b1; merging them into one Secret for convenience would quietly undo that.
- **`--from-env-file`, not `--from-literal`.** A literal lands in the process's argv, which is world-readable through `/proc` on a shared host. The script writes a `0600` temp file, uses it, and traps its removal. The certs go in by path for the same reason.

Also carried across deliberately unchanged: **`REDIS_PASSWORD` is still not wired up**. The Goal T12 audit found it documented but connected to nothing; the honest state is "Redis is unauthenticated inside the cluster network". Adding `--requirepass` only in the Kubernetes manifests would fork the two targets' security posture and make the gap harder to see, so the manifests carry the same gap and say so in a comment.

## The ConfigMap that Kustomize could not build

Postgres is initialised from `services/cloud/db/schema.sql` — the same file `docker-compose.yml` mounts into `/docker-entrypoint-initdb.d/`. The obvious move, a `configMapGenerator` with `files:`, is impossible: **Kustomize refuses to read files above its own root**, and the schema lives four directories away. Copying it under `infra/k8s/` would create a second source of truth that drifts from the Compose path — a schema difference between two deployment targets that only surfaces as a runtime SQL error.

So `scripts/k8s/apply.sh` creates that one ConfigMap imperatively from the canonical file. It is *not* hash-suffixed, on purpose: `/docker-entrypoint-initdb.d/` runs **only on first init of an empty PGDATA**, exactly as under Compose, so rolling the pod when the schema changes would imply an effect it does not have. Applying a new schema means wiping the volume — `RESET_DATA=1 ALLOW_DESTRUCTIVE=1 make k8s-apply`, the same double opt-in `scripts/deploy/smoke.sh` uses.

Everything *else* environment-shaped goes through a generated ConfigMap (`automail-config`) precisely because generators hash their content and rewrite the references, so changing a value rolls the pods instead of leaving them running the old one.

## The honest caveat (the follow-up an interviewer asks)

> *"So the database is highly available now?"*

No — and nothing here claims it. One replica, one volume, no replication, on a node-local disk. What Kubernetes added is **automatic pod-level recovery** (the process is restarted and the volume reattached without a human) and a **declarative** description of the desired state. Postgres HA is a different project: streaming replication plus a failover controller (Patroni, CloudNativePG), a decision about synchronous vs asynchronous commit, and an answer for split-brain. The StatefulSet is the correct *object* for that future; it is not the feature.

A second caveat worth volunteering: the four "nodes" are four containers on one kernel and one disk. Rescheduling is real, the scheduler's decisions are real — but node failure is simulated, and `local-path` means a genuinely lost node is genuinely lost data.

## Related

- [14-redis-streams-consumer-groups.md](14-redis-streams-consumer-groups.md) — what the Redis state is actually load-bearing for.
- [24-graceful-shutdown-consumer-lifecycle.md](24-graceful-shutdown-consumer-lifecycle.md) — why the stateless tier can be restarted freely and this tier cannot.
- [25-k3d-cluster-image-supply.md](25-k3d-cluster-image-supply.md) — the substrate these objects land on.
- [04-minio-sse-s3-vs-e2ee.md](04-minio-sse-s3-vs-e2ee.md) — why MinIO gets a KMS key even though the blobs are already end-to-end encrypted.
