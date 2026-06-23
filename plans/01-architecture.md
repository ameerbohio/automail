# Architecture

## System Diagram

```
┌─────────────────────────────────────────────────────────────────┐
│  SENDER BROWSER                                                  │
│  Web Crypto API: AES-256-GCM + RSA-OAEP                        │
│  Plaintext never leaves this boundary                           │
└──────────────────────────┬──────────────────────────────────────┘
                           │ HTTPS (TLS 1.3)
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│  TRAEFIK  (reverse proxy / load balancer)                       │
│  TLS termination · rate limiting · security headers             │
└─────────────────────┬───────────────┬───────────────────────────┘
                      │               │
              ┌───────▼──────┐ ┌──────▼──────────────────────────┐
              │  Next.js     │ │  Cloud Server — Go  ×N nodes     │
              │  Portal      │ │  [TRUST BOUNDARY: zero knowledge] │
              │  [SIMPLE]    │ │  stores/routes ciphertext only    │
              └──────────────┘ └─────────┬──────────────┬─────────┘
                                         │              │
                               ┌─────────▼──┐  ┌───────▼──────┐
                               │ PostgreSQL  │  │    Redis      │
                               │ job state   │  │ Streams       │
                               │ audit log   │  │ Pub/Sub       │
                               │ pgcrypto PII│  │ printer cache │
                               └────────────┘  └───────────────┘
                                         ▲
                       persistent mTLS WebSocket — printer dials out,
                          cloud pushes dispatch down the open socket
                                         │
                           ┌─────────────┴──────────────────────┐
                           │  Printer Microservice — Go  ×1      │
                           │  [TRUST BOUNDARY: decryption occurs] │
                           │  RAM only · tmpfs · CUPS · wipe      │
                           └─────────────┬──────────────────────┘
                                         │
                                 ┌───────▼──────┐
                                 │    MinIO      │
                                 │ encrypted     │
                                 │ blobs (SSE-S3)│
                                 └───────────────┘
                                         │
                                    Printer / CUPS
                                  (dev: file on disk)
                                         │
                                    Mailbox Slot N
```

> **Diagram terminology:** API = Application Programming Interface; AES-256-GCM = Advanced Encryption Standard 256-bit Galois/Counter Mode; RSA-OAEP = Rivest–Shamir–Adleman with Optimal Asymmetric Encryption Padding; HTTPS = Hypertext Transfer Protocol Secure; TLS = Transport Layer Security; Pub/Sub = publish/subscribe (a messaging pattern where senders broadcast to topics and receivers subscribe); PII = Personally Identifiable Information; mTLS = mutual Transport Layer Security (both parties present certificates, not just the server); RAM = Random-Access Memory; tmpfs = temporary file system (a RAM-backed filesystem — data is never written to disk); CUPS = Common Unix Printing System; SSE-S3 = Server-Side Encryption using S3-compatible key management (encrypts stored objects at rest in MinIO — note: "SSE" used later in the document means Server-Sent Events, a different concept).

## Trust Boundaries

| Boundary | What crosses it | What does not |
|---|---|---|
| Browser → Cloud Server | Ciphertext blob ref, encrypted AES (Advanced Encryption Standard) key, metadata | Plaintext document, raw AES key |
| Cloud Server → Printer | Encrypted AES key, time-limited MinIO read URL | Plaintext document |
| Printer → MinIO | Fetch ciphertext blob | Nothing is written back |
| Printer RAM | Decrypted PDF (Portable Document Format; in-memory only) | Disk, logs, network |

The cloud server is explicitly **zero-knowledge**: every field it stores or forwards is either ciphertext or metadata. It cannot reconstruct the plaintext document even if its database is fully compromised.

## Dispatch Flow (numbered)

Authentication is optional. A guest sender receives a one-time `guest_token` in the job response to track their submission without an account. An authenticated sender's job is linked to their account for history. Both paths go through the same encryption and dispatch pipeline; the difference is only in identity and persistence.

```
1.  Browser generates AES-256-GCM (Advanced Encryption Standard 256-bit Galois/Counter Mode) key (window.crypto.subtle)
2.  Browser encrypts PDF (Portable Document Format) → ciphertext blob
3.  Browser looks up recipient by address/name on cloud server → receives { recipient_id, display_name, building_address }
3b. Browser fetches printer's RSA (Rivest–Shamir–Adleman) public key from cloud server using recipient_id
      (server resolves recipient → slot → printer internally; sender never sees slot_id or mailbox_id)
4.  Browser RSA-OAEP (Optimal Asymmetric Encryption Padding) encrypts the AES key with printer's public key
5.  Browser requests pre-signed MinIO upload URL from cloud server (sends recipient_id; server resolves printer internally)
6.  Browser uploads ciphertext blob directly to MinIO over HTTPS (Hypertext Transfer Protocol Secure)
7.  Browser POSTs job to cloud server (Bearer JWT optional):
      { encrypted_key, blob_ref, recipient_id, page_count }
      (server resolves recipient_id → slot_id + mailbox_id internally)
      Authenticated response: { job_id, status }
      Guest response:         { job_id, status, guest_token }  ← one-time token to track this job
    (Precondition: at boot the printer dialed out and holds a persistent mTLS
     WebSocket to one cloud node — the "owner node"; that node is subscribed to
     mailbox:<id>:dispatch on Redis and caches the printer's state.)
8.  Cloud server stores job → reads printer state from Redis cache
9a. Printer idle + slot has capacity:
      Claim job in Postgres (SELECT FOR UPDATE NOWAIT) → status dispatching
      PUBLISH to mailbox:<id>:dispatch → { job_id, encrypted_key, pre-signed MinIO read URL }
      Owner node receives it and writes a dispatch frame down the open WebSocket
9b. Printer busy, slot full, or no live socket (zero subscribers):
      XADD job to Redis Stream
      Job status → queued
10. Printer fetches ciphertext blob from MinIO → RAM (Random-Access Memory) only
11. Printer RSA-OAEP decrypts AES key using its private key
12. Printer AES-256-GCM decrypts PDF in RAM
13. Printer writes decrypted PDF to tmpfs (temporary file system; /dev/shm)
14. Printer sends to CUPS (Common Unix Printing System) via `lp` command
15. CUPS confirms job accepted → printer unlinks tmpfs file
16. Printer zeroes all in-memory byte slices
17. Printer sends status frame { job_id, status: delivered } up the same WebSocket
18. Owner node updates job → publishes to Redis pub/sub (publish/subscribe) → relays via SSE (Server-Sent Events) to browser
19. Cloud server requests MinIO blob deletion (or schedules per reprint policy)
20. Printer sends state frame { status: idle, slot_occupancy: {...} } up the socket; ping/pong keepalive continues
21. If jobs are queued: dispatcher picks next eligible job, repeats from step 9a
```

## Docker Compose Topology

All services run in a single Docker Compose stack on the Proxmox VM (Virtual Machine).

```
docker-compose.yml
├── traefik          (ports 80, 443 exposed to LAN (Local Area Network))
├── cloud-server     (internal only, scaled via --scale)
├── printer-service  (internal only, one instance)
├── portal           (Next.js, internal, routed via Traefik)
├── postgres         (internal only, named volume)
├── redis            (internal only, named volume)
└── minio            (internal + optional LAN port for MinIO console)
```

Internal services communicate on a Docker bridge network. Only Traefik has external port bindings.

## Production Considerations: Field-Deployed Printers

> **What the demo implements:** The printer dials out and holds a persistent mTLS WebSocket to the cloud server, and the cloud server pushes dispatch over that open socket — the **same dispatch model used in production**. The only thing the demo simplifies is the *network path*: here the socket runs over the internal Docker bridge instead of the public internet. The dispatch code, the connection registry, and the cross-node routing are real and unchanged between demo and production.

### The NAT Problem (why the printer dials out)

In production each printer unit is a small Linux computer (Raspberry Pi class) physically co-located with a mailbox bank. It connects to the internet via a building's ISP connection, placing it behind NAT with a private IP (`192.168.x.x`). The cloud server has no route to initiate a connection to that address.

A naive "cloud server POSTs to printer" model would break:

```
Naive model (cloud dials printer):
Cloud Server ──mTLS POST──► ???      ✗  no route to a NAT'd private IP

Adopted model (printer dials cloud):
Printer ──persistent mTLS WebSocket──► Cloud Server   ✓  outbound works through NAT
        ◄──── dispatch pushed down the open socket ────
```

### Solution: Persistent mTLS WebSocket (Printer Dials Out) — implemented

Since the printer can make outbound connections through NAT, the model inverts: the printer opens a persistent mTLS WebSocket to the cloud server at startup and holds it open. The cloud server pushes dispatch messages through the established connection. **This is implemented in the demo** (see [04-printer-microservice.md](04-printer-microservice.md) and [05-cloud-server.md](05-cloud-server.md)); production differs only in the network path and the operational concerns below.

```
[Printer boots]
  └─ dials out: mTLS WebSocket → cloud.automail.example.com
  └─ sends: { mailbox_id, slot_occupancy }  ← registers presence

[Cloud server — owner node]
  └─ holds registry: mailbox_id → active WebSocket connection (in-process, this node)
  └─ subscribes to Redis mailbox:<id>:dispatch so any node can route a job here
  └─ on dispatch from any node: writes to that connection instead of POSTing

[Printer receives job over open socket]
  └─ fetches blob from MinIO (outbound HTTPS, works through NAT)
  └─ decrypts, prints, wipes
  └─ sends status back over same socket: { job_id, status: delivered }
```

The mTLS certificates are unchanged — the printer still presents its cert, the cloud still verifies it. Only the direction of the initial dial changes. The heartbeat becomes a WebSocket ping/pong keepalive; a dropped connection is itself the offline signal, which is cleaner than a missed heartbeat POST.

### Dispatch Transport at Scale: Push (demo) vs Poll (production)

The persistent-socket model above is **low-latency but not power-optimal at scale**. The two modes are a deliberate, switchable choice:

| | Persistent push (demo) | Jittered poll (production target) |
|---|---|---|
| Latency | seconds | minutes (acceptable for mail) |
| Device power | radio/CPU always on | wake → check → sleep |
| Server | holds N live mTLS sockets + keepalives | fully stateless; no registry/fan-in |
| Best when | few units, real-time UX | **12M units, power/opex-driven** |

At the production target (~12M shared-mailbox units serving ~30M people, latency-tolerant), **power is the dominant operating cost**, so units should **poll on a jittered interval** rather than hold a socket open: wake, mTLS-handshake (with **TLS session resumption** to skip the asymmetric step on repeat polls), pull any queued jobs, sleep. Jitter spreads the 12M wake-ups to avoid a thundering herd.

This demo runs the **persistent push** mode for low latency and to exercise the cross-node fan-in design. Production runs **poll** mode. The printer and cloud server are written so the transport is a swappable layer (`DISPATCH_MODE = push | poll`) over the same job/queue/crypto pipeline — only the delivery hop differs; dispatch eligibility, the Redis Stream queue, and decryption are identical in both.

### Additional Production Concerns

**Physical security.** The Raspberry Pi sits inside a mailbox cabinet in a building lobby — accessible to anyone with a screwdriver. The private key file is encrypted at rest (passphrase from env var, already planned), but a determined attacker with physical access can extract storage. Mitigations: secure boot, encrypted root filesystem (e.g. dm-crypt), tamper-evident enclosure. The E2EE design limits the damage — physical compromise of one printer unit exposes only jobs destined for that unit, not the entire system.

**Certificate rotation.** In Docker Compose, cert rotation is a `docker compose restart`. For field devices it requires a secure OTA (over-the-air) update mechanism with defined expiry windows and rollback capability. This is an operational engineering problem outside the scope of this prototype.

**Printer identity and registration.** In the prototype, a printer's public key is inserted into the database manually during setup. At scale, provisioning a new field unit needs a secure enrollment flow — the device generates its keypair on first boot, presents a one-time provisioning token (issued during manufacturing or setup), and the cloud server registers the public key tied to a physical unit ID. This is analogous to device attestation in IoT platforms.

### What the Prototype Keeps Internal (and what it does not)

The demo **implements** the dial-out dispatch model end to end — the connection registry, the per-printer Redis routing channel, the cross-node fan-in, ping/pong liveness, and reconnect-with-backoff are all real. This is deliberate: the NAT inversion is not merely a networking detail, it is a genuine distributed-systems problem (delivering to a stateful connection that lives on one node of a stateless cluster), and solving it is part of the engineering story rather than a gap to wave away.

What the demo simplifies is only what does **not** change the dispatch code:

- **Network path** — the WebSocket runs over the internal Docker bridge instead of the public internet. Same frames, same mTLS, same registry logic.
- **Certificate rotation** — handled by `docker compose restart`; production needs secure OTA with expiry/rollback (below).
- **Printer enrollment** — public keys are inserted manually; production needs a provisioning-token enrollment flow (below).

The security showcase the demo focuses on is unaffected and still front-and-centre:

- Browser-side E2EE (Web Crypto API — AES-256-GCM + RSA-OAEP)
- Zero-knowledge cloud server design
- mTLS on every internal hop, including the printer link
- Secure wipe and tmpfs handling on the printer

---

## Component Responsibility Summary

| Component | Language | Role |
|---|---|---|
| Cloud Server | Go | Zero-knowledge routing, job lifecycle, dispatch, SSE relay |
| Printer Microservice | Go | Decrypt in RAM, print, wipe, heartbeat |
| Portal | TypeScript (Next.js) | Sender UI (User Interface), Web Crypto encrypt flow, SSE display |
| Ops Dashboard | TypeScript (Next.js) | Job queue view, printer health (minimal) |
| Traefik | — | TLS (Transport Layer Security), LB (Load Balancer), rate limiting, security headers |
| PostgreSQL | — | Job state, audit log, encrypted PII |
| Redis | — | Printer state cache, job queue (Streams), SSE pub/sub |
| MinIO | — | Encrypted blob storage |
