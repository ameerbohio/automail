# CUPS Host Setup — Phase 10 (Real Printing)

**Status: host prerequisites DONE (2026-07-20) — the remaining work is the code
side (Steps 2–4).** This is the owner checklist referenced by GOALS.md Goal 6.
It is a list of steps for a human to perform on the deployment host — the agent
does not guess at or apply host configuration. The owner completed and verified
Step 1 directly on the Proxmox VM (see the "Done" block below); Steps 2–5 (the
container image, compose wiring, and `DEV_MODE=false` flip) are the code changes
Goal 6 still covers.

## What is already done (in code)

The printer microservice **already contains the real print path**. In
`services/printer/print.go` the pipeline decrypts to `/dev/shm`, and step 6
shells out to:

```go
exec.Command("lp", "-d", printerName, "/dev/shm/automail-<job_id>.pdf").Run()
```

`DEV_MODE=true` skips **only** that one `lp` call (it logs "dev: would print"
instead). So "implementing Phase 10" is not a code-logic change — it is:

1. host + container configuration so that `lp` inside the container can reach a
   real printer, and
2. flipping `DEV_MODE=false` and setting `PRINTER_NAME`.

Two code/image changes are still needed and are called out below (the container
image has no `lp` binary yet, and the Dockerfile itself says so).

---

## First-deploy prerequisite: edge TLS certificate (not CUPS-specific)

> Documented here until Goal T12 lands `docs/deploy-checklist.md`; this step
> applies to every fresh host bring-up, printing or not.

On a fresh host, `docker compose up` brings up the Traefik front door with
`sniStrict: true` (`infra/traefik/dynamic.yml`). If no certificate matches the
routed hostnames (`automail.local`, `api.automail.local`), Traefik **hard-rejects
every TLS handshake** and the browser shows `ERR_SSL_UNRECOGNIZED_NAME_ALERT` —
the connection never reaches the portal. The fix is to generate the self-signed
edge cert **before** the first `up` (it is one of the `infra/certs/gen*.sh`
scripts):

```bash
./infra/certs/gen-edge-certs.sh   # writes infra/traefik/edge-{cert,key}.pem
```

Notes:
- The cert is **self-signed**, so browsers show a one-time "not secure" warning —
  expected and fine for this deploy target. Only a *hard* TLS rejection is a bug.
- It is a **separate trust domain** from the internal mTLS CA (`gen.sh`): the edge
  cert secures browser ↔ Traefik; the mTLS PKI secures cloud ↔ printer. Do not
  cross-wire them, and do not "fix" a missing edge cert by disabling `sniStrict`.
- `scripts/e2e/bootstrap.sh` generates it automatically, so `make test-e2e{,-full}`
  already have it; a plain production `docker compose up` needs the manual step
  above (or run `bootstrap.sh` once).

Verify after `up`:

```bash
curl -k --resolve automail.local:443:127.0.0.1 https://automail.local/ -o /dev/null -sS
curl -k --resolve api.automail.local:443:127.0.0.1 https://api.automail.local/healthz -o /dev/null -sS
# Both should complete the TLS handshake (self-signed, hence -k) rather than
# fail with an unrecognized_name alert.
```

---

## Step 1 — Configure CUPS on the Proxmox VM host — ✅ DONE (2026-07-20)

Completed and verified by the owner on the Proxmox VM that runs the Docker
stack. Confirmed configuration:

- **Printer:** Canon imageCLASS **MF240 Series** (UFRII LT), USB-attached.
- **Proxmox passthrough:** passed to the VM **by USB vendor:device ID
  `04a9:27d2`** (not by physical port), so it survives being unplugged and
  replugged.
- **CUPS + `cups-client`** installed on the VM host.
- **Queue name: `Canon_MF240`** — driverless **IPP-over-USB** via the `ipp-usb`
  bridge, backed by the generic **`-m everywhere`** IPP driver. No Canon vendor
  driver is needed.
- **Verified with repeated real PDF print jobs** (not just attribute queries):
  paper came out correctly multiple times in a row.
- **`PRINTER_NAME=Canon_MF240`** is the confirmed value for Step 4 / `.env`.

The generic recipe (for a different host/printer) is still: install
`cups cups-client`, add the queue (`lpadmin -p <name> -E -v <device-uri> -m
everywhere`, or the `https://localhost:631` web UI), test it from the host
(`echo test | lp -d <name>`), and read the exact queue name back with
`lpstat -p -d`. Do not proceed to the container steps until the **host itself**
prints — every later step assumes a working host queue.

### Troubleshooting notes from this bring-up

- **If this printer goes flaky, try this first (unconfirmed — not a required
  procedure).** Early on the device was non-deterministic — it worked maybe 1
  time in 5, sometimes returning a clean response and sometimes a bare "0 bytes
  / internal-error". It became reliable after: power-cycling the printer, doing
  a kernel-level USB deauthorize/reauthorize on the VM (toggling the device's
  `.../authorized` flag in sysfs), and restarting `ipp-usb`. That sequence was
  **not** rigorously isolated as the root cause — the flakiness might have
  cleared on its own — so treat it as a first thing to try if the printer
  misbehaves again, not as a documented fix.
- **No `ipp-usb` quirks entry is needed for this model (confirmed).** A missing
  quirks entry (mirroring the workaround `ipp-usb` ships for the Canon SELPHY
  CP1500) was suspected, but ruled out with a real A/B test: with a test quirk
  disabled, the reset/restart sequence still printed reliably. So no `ipp-usb`
  quirks file edit is required for the MF240.

---

## Step 2 — Add the CUPS client to the printer container image

The runtime image is currently bare `alpine` with **no `lp` binary** (see the
comment in `services/printer/Dockerfile`). Add the client package to the final
stage:

```dockerfile
FROM alpine:3.19
RUN apk add --no-cache cups-client        # provides `lp`, `lpstat`
COPY --from=build /out/printer /usr/local/bin/printer
EXPOSE 8444
ENTRYPOINT ["printer"]
```

Rebuild: `docker compose build printer`.

---

## Step 3 — Expose the host CUPS to the container

`lp` in the container needs to talk to the host's `cupsd`. Pick **one**:

**Option A — mount the host CUPS socket (simplest for a single-host demo).**
In `docker-compose.yml` under the `printer` service:

```yaml
    volumes:
      - ./infra/certs:/certs:ro
      - /run/cups/cups.sock:/run/cups/cups.sock      # host cupsd socket
```

The container's `lp` uses the default socket automatically. Confirm the host
socket path (`ls -l /run/cups/cups.sock`); some distros use `/var/run/cups/`.

**Option B — TCP CUPS.** Configure the host `cupsd` to listen on `631` and
allow the Docker bridge subnet (edit `/etc/cups/cupsd.conf`: a `Listen
0.0.0.0:631` / `Port 631` line plus an `Allow` for the bridge CIDR), then set an
env var on the container:

```yaml
    environment:
      CUPS_SERVER: "host.docker.internal:631"   # or the host's bridge-gateway IP
```

Option A keeps CUPS off the network and is preferred for the demo. Option B is
closer to how a real field unit (separate host) would reach a print server.

---

## Step 4 — Flip the printer service to production mode

In `docker-compose.yml` (or `.env`) for the `printer` service:

```yaml
      DEV_MODE: "false"
      PRINTER_NAME: "Canon_MF240"     # exactly the queue name from Step 1
```

`PRINTER_NAME` is already plumbed through (`services/printer/config.go`) and is
the `-d` argument to `lp`. With `DEV_MODE=false` and a reachable queue, step 6
of the pipeline prints for real.

---

## Step 5 — Verify (roadmap Phase 10 acceptance)

1. `docker compose up -d --build`
2. Submit a job end-to-end (guest portal or the admin/sender flow).
3. **Paper comes out of the printer with the correct document content.**
4. Confirm no plaintext remains:
   ```sh
   docker compose exec printer ls -la /dev/shm    # no automail-*.pdf files
   ```
   The service unlinks the tmpfs file **before** reporting `delivered`
   (`print.go` step 7), so a delivered job must leave `/dev/shm` empty.

---

## Security note the owner must decide on (important)

The zero-knowledge invariant is "plaintext lives only in printer RAM + tmpfs,
unlinked before the status callback." Introducing real CUPS adds a wrinkle:

- **CUPS spools the job to disk by default.** When `lp` submits the tmpfs PDF,
  `cupsd` copies it into its spool directory (`/var/spool/cups/` on the CUPS
  host) as it rasterizes/queues. That is **plaintext written to persistent disk
  on the host** — outside the RAM-only guarantee — for the lifetime of the spool
  file.
- Mitigations to choose from:
  - Set `PreserveJobFiles No` and `PreserveJobHistory No` in `cupsd.conf` so
    CUPS deletes spool files immediately after printing (shrinks but does not
    eliminate the on-disk window).
  - Mount the CUPS spool directory on a `tmpfs` (RAM-backed) so the spool never
    touches persistent storage — the closest match to the invariant.
  - Accept a brief on-disk spool as a documented, bounded risk for the
    prototype.

This is a genuine design decision (which is why Phase 10 is owner-gated), not
something to default silently. Whichever option is chosen should be recorded in
`plans/02-security.md` so the invariant text and the implementation agree.

---

## Summary of changes required

| Where | Change | Status |
|---|---|---|
| Proxmox VM host | Install CUPS, add + test the physical printer queue (`Canon_MF240`) | ✅ done 2026-07-20 |
| `services/printer/Dockerfile` | `apk add --no-cache cups-client` in the final stage | pending (Goal 6) |
| `docker-compose.yml` (printer) | Mount `/run/cups/cups.sock` **or** set `CUPS_SERVER` | pending (Goal 6) |
| `docker-compose.yml` / `.env` | `DEV_MODE=false`, `PRINTER_NAME=Canon_MF240` | pending (Goal 6) |
| CUPS config (owner decision) | Spool-to-disk hardening per the security note | pending (owner decision) |

No changes to `print.go`'s logic are required — the `lp` invocation, tmpfs
write, and unlink-before-delivered are already implemented and unit-tested.
