# V2 Roadmap — Deferred Work & Future Ideas

> **Scope banner.** This file is **not** part of the V1 specification. Everything
> in `plans/00`–`plans/12` describes the system that is being built now (V1: the
> end-to-end encrypted demo with the real dispatch/crypto/security model). This
> file is the **parking lot for V2** — ideas the author intends to build *later*
> and deliberately does **not** want in V1. Nothing here is goal-tracked in
> `GOALS.md`, and nothing here should be implemented as part of a V1 phase or
> testing goal. If an item graduates into active work, promote it into a proper
> `plans/` section (+ a roadmap phase) and remove it from here.

## How to use this doc

Add each idea as its own `##` section with this shape, so a cold reader (or a
future agent) can pick it up without re-deriving the context:

- **What** — the change in one or two sentences.
- **Why V2, not V1** — what makes it out of scope for the current build.
- **Current V1 state** — what exists today that this builds on or replaces.
- **Sketch / considerations** — enough of a design hint to resume from, and the
  hard parts to watch.
- **References** — the `plans/` sections and code that are relevant.

---

## Separated field-unit deployment (printer node against a remote cloud)

- **What.** Deploy a printer microservice as an independent field unit on its own
  hardware (Raspberry-Pi-class box at a mailbox bank), dialing a **remote,
  already-running** cloud over the public internet — rather than as a co-located
  Docker Compose service on the same host as the cloud. Includes per-unit
  identity and mTLS certificate **provisioning** (each unit gets its own client
  cert, `MAILBOX_ID`/`SLOT_ID`, and its document keypair), and the operational
  path to enrol / revoke a unit.

- **Why V2, not V1.** V1's goal is to demonstrate the whole system end-to-end on
  one host; the printer sits in the compose stack purely as a dev convenience.
  The dispatch *mechanism* is already the production one — only the network path
  differs. Splitting the printer onto separate hardware adds fleet provisioning,
  public-internet TLS/DNS, and per-unit secret management that V1 doesn't need to
  prove the concept.

- **Current V1 state.** The printer binary already has **no co-location
  assumption**: identity, the cloud endpoint (`CLOUD_SERVER_WS_URL`), and its
  mTLS cert are all env/file config, so the same image that runs in compose is
  what a field unit runs — only the values differ. The dial-out WebSocket,
  per-node connection registry, and Redis cross-node dispatch routing are
  implemented and proven (roadmap Phases 3–4; the two-node fan-in/fan-out is
  exercised by Testing Goal T8 / `make test-e2e-full`).

- **Sketch / considerations.**
  - Point `CLOUD_SERVER_WS_URL` at the public cloud (`wss://cloud.<domain>/internal/printer-link`); the printer accepts no inbound connections, so only its one outbound socket + local healthcheck exist.
  - Per-unit provisioning is the real new work: issuing/rotating client certs against the internal CA, seeding the unit's `mailboxes`/`mailbox_slots` rows and its document public key, and a revocation path for a lost/tampered unit.
  - Not covered by Testing Goal **T12** (deploy parity), which only brings up the *whole stack* on one Proxmox host — it does not cover the separated field host or the public network path.

- **References.** `plans/01-architecture.md` "Production Considerations:
  Field-Deployed Printers"; `plans/04-printer-microservice.md` "Dispatch Model:
  Printer Dials Out" + Configuration; `plans/11-ai-collaboration.md` (NAT problem
  `[accepted]`, dispatch inversion `[implemented in demo]`).

---

## Swappable dispatch transport: `DISPATCH_MODE = push | poll`

- **What.** Make the printer↔cloud dispatch transport a swappable layer over a
  shared pipeline: keep the persistent-push WebSocket for the demo, but add a
  **jittered-polling** mode as the production model for fleet scale.

- **Why V2, not V1.** At the real target (~12M shared-mailbox units, ~30M
  people), holding a persistent socket per unit doesn't scale: the server-side
  cost of maintaining millions of open sockets and the per-device radio/CPU power
  of an always-on connection make **jittered polling** the right production model
  (with TLS session resumption to avoid repeat asymmetric handshakes). V1 keeps
  persistent push because it gives low latency and showcases the cross-node
  fan-in — the interview point. This is a decided design intent that was **never
  specced into `plans/03` nor implemented**.

- **Current V1 state.** Only persistent push exists (`link.Hub` + the printer's
  dial-out read loop). There is no `DISPATCH_MODE` config, no polling path, and
  no shared abstraction between the two.

- **Sketch / considerations.**
  - Factor the dispatch pipeline (eligibility → claim via `FOR UPDATE NOWAIT` → deliver) so the transport is pluggable; push writes to the held socket, poll answers a printer's authenticated "any jobs for `<mailbox_id>`?" request.
  - Power efficiency is the primary operating-cost lever — polling cadence + jitter is the tuning knob; latency tolerance is the trade.
  - TLS session resumption matters at this scale to avoid a full handshake per poll.

- **References.** `plans/03-scaling.md` (dispatch model); `plans/11-ai-collaboration.md`
  (the push-vs-poll decision at 12M-unit scale).

---

## More V2 ideas (unfleshed)

Drop quick one-liners here as they come up; flesh them into full sections above
when they get closer to real work.

- _(add ideas here)_
