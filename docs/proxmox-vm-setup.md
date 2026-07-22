# Proxmox VM Setup — Automail Deployment Host

Handoff for provisioning the VM that runs the Automail Docker stack on Proxmox.
Starting state: Proxmox web UI is up, **no VMs exist yet**. Automail runs as a
Docker Compose stack **inside one Linux VM** (not an LXC container — Docker in a
VM is cleaner and avoids LXC nesting quirks).

This doc covers **provisioning the VM and bringing the stack up in `DEV_MODE`**.
The physical-printer / CUPS layer is a separate, owner-gated step — see
[cups-host-setup.md](cups-host-setup.md). Until that is done the printer service
stays `DEV_MODE=true` (full crypto pipeline runs; only the physical `lp` call is
skipped), so the stack is fully functional end-to-end except paper coming out.

---

## 1. Get an OS ISO onto Proxmox

Recommended guest OS: **Ubuntu Server 24.04 LTS** (Debian 12 also fine). Server,
not desktop — no GUI needed.

In the Proxmox web UI:
1. Left tree → your node → **local** (or whichever storage holds ISOs) → **ISO Images**.
2. **Download from URL** and paste the Ubuntu Server 24.04 LTS ISO URL
   (`https://releases.ubuntu.com/24.04/ubuntu-24.04-live-server-amd64.iso`), or
   **Upload** a downloaded ISO.

---

## 2. Create the VM (Create VM wizard, top-right)

| Wizard tab | Setting | Value | Why |
|---|---|---|---|
| **General** | Name | `automail` | — |
| **OS** | ISO image | the Ubuntu ISO from step 1 | — |
| | Type / Version | Linux / 6.x - 2.6 Kernel | — |
| **System** | Machine | `q35` | modern PCIe; needed if USB passthrough is added later for the printer |
| | BIOS | OVMF (UEFI) or SeaBIOS | either works; SeaBIOS is simplest |
| | QEMU Agent | **checked** | lets Proxmox read the VM's IP and do clean shutdowns; install `qemu-guest-agent` in the guest |
| **Disks** | Bus/Device | VirtIO Block or SCSI (VirtIO SCSI single) | best perf |
| | Disk size | **40 GB** (min 32) | Docker images (postgres, redis, minio, traefik, node build layers) + volumes |
| | Discard + SSD emulation | checked (if backing store is SSD) | trims freed blocks |
| **CPU** | Cores | **4** (min 2) | Go + Next.js image builds are the heaviest moment |
| | Type | `host` | passes through host CPU features; fastest |
| **Memory** | RAM | **8192 MB** (min 4096) | Postgres + Redis + MinIO + 2 Go services + Next.js + Traefik; the Next.js build is the RAM spike |
| **Network** | Bridge | `vmbr0` (default) | — |
| | Model | VirtIO (paravirtualized) | — |

Finish, then **Start** the VM and open its **Console** to run the Ubuntu installer.

### Sizing summary
- **Minimum:** 2 vCPU / 4 GB RAM / 32 GB disk.
- **Comfortable (recommended):** 4 vCPU / 8 GB RAM / 40 GB disk.

---

## 3. Install Ubuntu (in the VM console)

Standard Ubuntu Server install. During install:
- **Install OpenSSH server** (checkbox) so you can leave the console and use SSH.
- Note the VM's **IP address** (shown at login, or `ip a`). Give it a **static
  IP** or a DHCP reservation — the stack is reached by hostname, and you don't
  want the IP moving. Call this `<VM_IP>` below.

After first boot:
```sh
sudo apt-get update && sudo apt-get install -y qemu-guest-agent git
sudo systemctl enable --now qemu-guest-agent
```

---

## 4. Install Docker Engine + Compose plugin

```sh
# Docker's official convenience script (Engine + compose plugin)
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker "$USER"
# log out / back in (or `newgrp docker`) so the group takes effect
docker compose version   # confirm the compose plugin is present
```

---

## 5. Get the code and generate secrets

Automail keeps **all** secrets out of git (`.env`, `*.pem`, `certs/` are
gitignored). Nothing sensitive is in the repo — everything is generated on the
host at first bring-up.

```sh
git clone <automail-repo-url> automail && cd automail

# 1. mTLS PKI (CA + cloud-server + printer certs)
./infra/certs/gen.sh
# 2. JWT RS256 signing keypair
./infra/certs/gen-jwt-keys.sh
# 3. Printer document-decryption keypair (RSA-4096, encrypted at rest)
PRINTER_KEY_PASSPHRASE='<choose-a-strong-passphrase>' ./infra/certs/gen-printer-keys.sh
# 4. Browser-facing edge TLS cert -- REQUIRED. Traefik runs with sniStrict, so
#    without it every TLS handshake is hard-rejected (ERR_SSL_UNRECOGNIZED_NAME_ALERT).
./infra/certs/gen-edge-certs.sh

# 5. Environment
cp .env.example .env
```

Then edit `.env` and replace every `changeme*` value with a real secret. The
required ones (from `.env.example`):

| Var | What |
|---|---|
| `POSTGRES_PASSWORD` | Postgres superuser password |
| `REDIS_PASSWORD` | Redis password |
| `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` | MinIO console + S3 creds |
| `MINIO_KMS_SECRET_KEY` | `automail-sse-key:$(openssl rand -base64 32)` — MinIO SSE-S3 key |
| `APP_ENCRYPTION_KEY` | pgcrypto key for encrypted PII columns (email/name) |
| `PRINTER_KEY_PASSPHRASE` | **must match** the passphrase used in `gen-printer-keys.sh` above |
| `DEV_MODE` | keep `true` until the CUPS host setup is done |

Generate strong values with `openssl rand -base64 32`. **Do not commit `.env`
or anything under `infra/certs/`.**

---

## 6. Hostname routing (Traefik uses Host headers)

Traefik routes by hostname, not path:
- `automail.local` → the portal (web UI)
- `api.automail.local` → the cloud API
- `blob.automail.local` → object storage (the browser uploads the ciphertext
  straight there via a pre-signed URL, so it must resolve too — see
  [deploy-checklist.md §4](deploy-checklist.md))

So the machine whose **browser** opens the app must resolve those names to
`<VM_IP>`. Two options:
- **Quick:** add to the *client* machine's hosts file
  (`C:\Windows\System32\drivers\etc\hosts` on Windows, `/etc/hosts` on Linux):
  ```
  <VM_IP>  automail.local  api.automail.local  blob.automail.local
  ```
- **Proper:** add both A records to your LAN DNS pointing at `<VM_IP>`.

The stack terminates TLS with the self-signed CA from `infra/certs/`, so the
browser will warn about an untrusted cert — expected for the prototype; accept
it, or import `infra/certs/ca-cert.pem` as a trusted root.

---

## 7. Bring the stack up

```sh
docker compose up -d --build     # first build pulls images + compiles Go/Next.js
docker compose ps                # all services healthy
```

Ports the VM exposes to the LAN: **80** and **443** (Traefik). Everything else
(Postgres 5432, Redis 6379, MinIO 9000/9001, cloud 8080/8443, printer 8444) is
internal to the `automail` Docker network and is **not** published — by design.

### Host-CPU compatibility (MinIO `x86-64-v2`)

**Old host CPUs need MinIO's `-cpuv1` image — already pinned in
`docker-compose.yml`, so no action is normally needed. Read this before ever
bumping the MinIO tag.**

Since `RELEASE.2023-11-01T01-57-10Z`, MinIO's *default* Docker image is based on
RHEL9/UBI9, whose glibc **hard-requires the `x86-64-v2` micro-architecture
baseline** (SSE4.2, POPCNT, …). Deploy hosts with a CPU older than ~2013 don't
have those instructions. On such a host the default image crashes on boot with:

```
minio-1  | Fatal glibc error: CPU does not support x86-64-v2
```

and because `cloud-server` `depends_on` `minio`, that single crash takes the
whole stack down (`docker compose ps` shows both `minio` and `cloud-server`
absent while postgres/redis/printer/portal/traefik are up).

- **This is not a Proxmox setting.** The recommended CPU Type `host` (§2) passes
  the *real* host CPU through, so a VM on an old host inherits the missing
  instructions. No QEMU CPU model can add instructions the physical CPU lacks.
- **Known-affected target:** the AMD A6-3600 "Llano" (2011, FM1) — the CPU this
  project's Proxmox host runs.
- **Fix (already applied):** `docker-compose.yml` pins the MinIO service to a
  **`-cpuv1`** image (`minio/minio:RELEASE.2025-09-07T16-13-09Z-cpuv1`). MinIO
  publishes this parallel variant on a v1-compiled base for exactly these CPUs
  (github.com/minio/minio issue #18365). It's the *same* MinIO — no feature or
  CVE-patch rollback — and it runs on modern CPUs too, so the pin is safe
  everywhere. **Do not "modernize" it back to `:latest` or a bare `RELEASE` tag**
  unless the deploy host is known to support `x86-64-v2`.
- **Verify on the host** after pulling this pin:
  ```sh
  docker compose up -d minio && docker compose logs minio
  # expect the startup banner, NOT "Fatal glibc error: CPU does not support x86-64-v2"
  ```

To check a host's level directly: `/lib64/ld-linux-x86-64.so.2 --help | grep -A2 'Subdirectories'` (look for `x86-64-v2 (supported/unsupported)`), or inspect `/proc/cpuinfo` for the `sse4_2` and `popcnt` flags.

### Verify
0. `make deploy-smoke` — automates every check below plus the edge TLS/SNI,
   security-header and pre-signed-upload assertions. See
   [deploy-checklist.md §7](deploy-checklist.md).
1. Browse to `https://automail.local` → portal loads.
2. Run the guest flow (upload a PDF → get a guest token → watch `/track` go
   `submitted → dispatching → printing → delivered`). In `DEV_MODE` the printer
   logs `dev: would print` instead of emitting paper — everything else is real.
3. Confirm no plaintext leaks: `docker compose exec printer ls -la /dev/shm`
   shows no `automail-*.pdf` after a delivered job.

---

## 8. Physical printing (owner-gated — later)

Turning on real paper output is a **separate step** documented in
[cups-host-setup.md](cups-host-setup.md): install CUPS in the VM, add the
printer queue, add `cups-client` to the printer image, expose the CUPS socket to
the container, then set `DEV_MODE=false` + `PRINTER_NAME`.

**Proxmox-specific wrinkle:** if the printer is **USB-attached** to the Proxmox
host, the VM can't see it until you pass it through: VM → **Hardware** → **Add**
→ **USB Device** → pick the printer (by vendor/device ID, not port, so it
survives re-plugging). A **network printer** needs no passthrough — the VM
reaches it over the LAN. There is also an open zero-knowledge decision about
CUPS spooling plaintext to disk (see the security note in
[cups-host-setup.md](cups-host-setup.md)). None of this is needed to run the
stack in `DEV_MODE`.

---

## Quick reference — what browser-Claude configures in the Proxmox UI

1. **local storage → ISO Images →** download Ubuntu Server 24.04 LTS.
2. **Create VM:** name `automail`, that ISO, q35, QEMU agent on, 40 GB disk
   (VirtIO), 4 cores type `host`, 8192 MB RAM, bridge `vmbr0` VirtIO NIC.
3. **Start → Console →** install Ubuntu (enable SSH), note the (static) IP.
4. Everything after that is inside the guest OS (steps 4–7 above), not the
   Proxmox UI — Proxmox's only remaining job is optional USB printer
   **passthrough** for real printing later (step 8).
