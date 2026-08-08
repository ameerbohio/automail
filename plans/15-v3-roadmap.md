# V3 Roadmap — Production-Ready, Geo-Distributed Fleet

> **Scope banner.** Further out than `plans/13` (V2). Staging, so it's legible when
> the author gets here:
> - **V1** (`plans/00`–`plans/12`): the end-to-end encrypted demo on one host.
> - **V2** (`plans/13`): multiple **cloud-server replicas on a trusted LAN** + printers
>   anywhere + a real multi-node deploy. The *code* for N cloud nodes already exists and
>   is tested — V2 is mostly the deployment/orchestration layer and hardware.
> - **V3** (this file): the step that makes the fleet **survive any single site
>   failing** — geo-distributed cloud nodes, a highly-available distributed data tier,
>   and a front door with no single point of failure.
>
> Nothing here is V1 or V2 work. Promote an item into a proper `plans/` section + a
> roadmap phase when it graduates, and remove it from here.

## How to use this doc

Same shape as `plans/13`: **What / Why V3, not V2 / Current state (what V2 leaves) /
Sketch & considerations / References.**

---

## The V2 → V3 boundary (why this is a separate stage)

V2 scales the **stateless cloud-server tier** — N replicas behind Traefik — but every
replica shares **one Postgres, one Redis, one MinIO** on a trusted LAN. That leaves
three single points of failure, and removing them is what V3 *is*:

1. **The shared data tier.** One Redis — now load-bearing for **correctness**, not just
   cache: every dispatch and status hop goes through its pub/sub. One Postgres primary.
   One MinIO.
2. **Site locality.** A cloud node genuinely off-LAN (a friend's house) can't reach the
   home data tier safely/quickly without either a secure overlay or a distributed data
   tier.
3. **The edge.** A single Traefik at one location is the fleet's only front door; if
   that site's link drops, the whole fleet is unreachable even if remote cloud nodes are
   healthy.

**Author's own framing (the reminder):** *"V3 would allow a cloud node at another
friend's house and the main Traefik at my place."* That first step — one remote cloud
node + a home edge — is a valid V3 entry point, but its honest end-state also removes the
edge SPOF (see below). And note the asymmetry established in V2: **printer nodes are
already allowed anywhere** (dial-out through NAT); it is the **cloud nodes** whose
location V3 unlocks.

---

## Distributed, highly-available data tier

- **What.** Redis Sentinel/Cluster, Postgres streaming replication + automatic failover,
  MinIO distributed mode — so no single data-store instance failing takes the fleet down.

- **Why V3, not V2.** V2 deliberately keeps one of each to stay simple; the
  horizontal-scaling story is already told by N stateless cloud replicas. HA data is a
  large, separate body of work (replication topology, failover, split-brain, consistency).

- **Current state (what V2 leaves).** Single Postgres/Redis/MinIO shared over the LAN.
  `notes/resume-cheatsheet.md` already flags the per-stream Redis-subscription cost and
  notes "at real scale you'd multiplex subscribers behind a shared fan-out."

- **Sketch / considerations.**
  - **Redis is the sharp edge** because its pub/sub is correctness-critical: a failover
    must not silently drop a dispatch/status message mid-flight. Design for
    at-least-once + idempotent job-state transitions (the Postgres job-claim already
    guards double-dispatch, so re-delivery should be safe if transitions are idempotent).
  - **Postgres failover** interacts with the job-claim atomicity — the claim must remain
    correct across a primary switch.
  - **MinIO distributed mode** needs enough nodes for erasure coding; plan node count up
    front.
  - Consider whether one global data tier is even right, or whether **regional data
    tiers + async replication** better fit a geo-distributed fleet (ties into the next
    item).

- **Discussions (open — not decided; resolve before implementing).** The framing to
  carry in: **active-active is not the universal goal here — one write path must stay
  single-authority for correctness.** So V3 is expected to be a *hybrid*, active-active
  where safe and active-passive where a single source of truth is load-bearing.
  - **Postgres — active-passive (single writer)?** Leaning yes. The exactly-once
    dispatch guarantee is the atomic **job-claim** (`UPDATE … status='dispatching'` /
    revert in `services/cloud/dispatch/route.go`), which assumes one authority deciding
    who claimed a job. Multi-master "write anywhere" (BDR/pglogical) reintroduces the
    double-dispatch race the claim exists to prevent — so single primary + automatic
    failover (Patroni/etcd), optional read replicas. **Open:** is the promote-on-failover
    window acceptable, and does anything besides the claim need write availability during
    it?
  - **Redis — Cluster (active-active, sharded) vs Sentinel (active-passive)?** Cluster
    gives multiple masters but each channel still has one owning master. **The real task,
    not a config flag:** dispatch uses `PUBLISH`/`SUBSCRIBE` on `dispatch:<mailbox_id>`,
    and classic pub/sub in Cluster broadcasts to every node. Moving to Cluster means
    **sharded pub/sub** (`SPUBLISH`/`SSUBSCRIBE`, Redis 7) with the channel keyed so a
    mailbox's publisher and its socket-owner subscriber land on the same shard. **Open:**
    Cluster+sharded-pubsub vs Sentinel's simpler active-passive — is the extra complexity
    worth it before geo-distribution actually forces it?
  - **MinIO — active-active.** Distributed mode is all-nodes read/write (erasure-coded),
    no promote step; multi-site via bucket replication. The easy one.
  - **One-liner to preserve:** *active-active everywhere except the write path that
    guarantees a job is dispatched exactly once — that stays single-authority on purpose.*

- **References.** `plans/03-scaling.md`; `services/cloud/dispatch/route.go` +
  `services/cloud/link/hub.go` (the Redis-mediated routing a distributed Redis must
  preserve); `notes/resume-cheatsheet.md` (subscription-cost note); `plans/13`
  (multi-node topology this builds on).

---

## Geo-distributed cloud nodes over a secure overlay

- **What.** Run cloud-server replicas across physically separate sites (e.g. a friend's
  house), each reaching the data tier — or a regional data tier — over a
  WireGuard/Tailscale mesh.

- **Why V3, not V2.** This crosses the **trusted-network** constraint V2 relies on
  (`plans/13`, "Cloud nodes must share a trusted network with the data tier"). It forces
  either the distributed data tier above or a WAN-latency-tolerant design.

- **Current state.** V2 confines cloud nodes to the LAN precisely because the data-tier
  connections aren't internet-safe.

- **Sketch / considerations.**
  - Overlay mesh so remote nodes see the data tier as if local — but watch **cross-WAN
    Redis latency on every dispatch hop**; a chatty pub/sub over the WAN is the failure
    mode.
  - Likely resolution: **regional data tiers + async replication** rather than one global
    Redis every remote node round-trips to.

- **References.** `plans/13` (trusted-network boundary + printer dial-out asymmetry);
  `plans/01-architecture.md` (dial-out, field-deployed printers); the data-tier item above.

---

## Edge / front-door HA (remove the Traefik SPOF)

- **What.** More than one public entry point with DNS/anycast failover, so no single
  site's Traefik being down makes the fleet unreachable.

- **Why V3, not V2.** V2 (and the author's first V3 step) run a single Traefik —
  acceptable to start, but it's the last SPOF once the data tier and cloud nodes are HA.

- **Current state.** One Traefik front door (`plans/01`, the demo stack).

- **Sketch / considerations.** Printers dial a **hostname, not an IP**, so DNS-based
  failover (health-checked multi-A, or a global LB) can migrate them between edges
  without touching the unit. Cloudflare — already used for the demo tunnel — can act as
  the health-checked global front door.

- **References.** `plans/01-architecture.md` (single edge today); `plans/13`
  (multi-node topology); `scripts/demo/up.sh` (Cloudflare tunnel already in use).

---

## More V3 ideas (unfleshed)

- Per-region job affinity / routing, so a printer's traffic stays in its region.
- Rolling upgrades across the fleet without dropping in-flight dispatches.
- Fleet-wide observability that spans sites (extends the `X-Automail-Node` header idea
  in `plans/13`).
