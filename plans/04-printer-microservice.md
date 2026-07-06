# Printer Microservice

**Language**: Go  
**Tag**: [CORE]  
**Role**: The only component that touches plaintext. Intentionally minimal — easy to audit.

---

## Responsibilities

1. Dial out to the cloud server on startup and hold a persistent mTLS WebSocket open
2. Register its presence (mailbox_id, slot occupancy) over that connection
3. Receive dispatch messages pushed down the open socket
4. Fetch the encrypted blob from MinIO
5. Decrypt: RSA-OAEP unwrap AES key → AES-256-GCM decrypt PDF — all in RAM
6. Print via CUPS (`lp` command) using a tmpfs temp file
7. Wipe all decrypted content from memory and disk
8. Send status updates back up the same socket
9. Keep the connection alive (ping/pong) and report state on change

---

## Dispatch Model: Printer Dials Out

The printer is a **WebSocket client**, not an HTTP server. It does not accept inbound connections for jobs. On startup it opens a single persistent mTLS WebSocket to the cloud server and holds it open; the cloud server pushes dispatch messages down that connection.

This matches the production field-deployment model (printers sit behind NAT and cannot accept inbound connections — see [01-architecture.md](01-architecture.md) "Production Considerations"). The demo and production now share one dispatch model; only the network path differs (internal Docker bridge here, public internet there). The mTLS certificates and verification are identical — the printer still presents its client cert and the cloud still verifies it against the internal CA. Only the *direction of the initial dial* is inverted versus a classic request/response API.

> **Transport is a swappable layer (`DISPATCH_MODE = push | poll`).** This demo uses `push` (persistent socket, low latency). At the 12M-unit production scale, power efficiency dominates operating cost, so units instead **poll on a jittered interval** (wake → mTLS handshake with TLS session resumption → pull queued jobs → sleep). Both modes share the same decrypt/print/wipe pipeline below; only the dispatch delivery hop changes. See [01-architecture.md](01-architecture.md) "Dispatch Transport at Scale".

### Connection lifecycle

```
[Printer boots]
  └─ dials wss://cloud-server:8443/internal/printer-link  (mTLS client cert presented)
  └─ sends register frame: { type: "register", mailbox_id, slot_occupancy }

[Steady state]
  └─ receives dispatch frames; processes; sends status frames back
  └─ ping/pong keepalive; pushes a state frame on slot-occupancy change

[Connection drops]
  └─ reconnect with exponential backoff + jitter, then re-register
  └─ (a dropped socket is itself the offline signal the cloud server observes)
```

The only locally bound port is a no-auth `GET /healthz` on `LISTEN_ADDR` for Docker's container healthcheck. It carries no job traffic.

### Message frames

All frames are JSON text messages discriminated by a `type` field.

**Server → printer**

```json
{ "type": "dispatch",
  "job_id": "uuid",
  "encrypted_key": "base64-encoded RSA-OAEP ciphertext",
  "blob_url": "https://minio.internal/automail/blobs/<ref>?X-Amz-Signature=..." }
```

`blob_url` is a pre-signed MinIO read URL (time-limited, single-use).

**Printer → server**

```json
{ "type": "register", "mailbox_id": "uuid",
  "slot_occupancy": { "<slot_id>": { "current": 2, "max": 5 } } }

{ "type": "state", "status": "idle",          // "idle" | "printing"
  "slot_occupancy": { "<slot_id>": { "current": 2, "max": 5 } } }

{ "type": "status", "job_id": "uuid",
  "status": "printing",                        // "printing" | "delivered" | "failed"
  "error": "optional message if failed" }
```

```go
type SlotInfo struct {
    Current int `json:"current"`
    Max     int `json:"max"`
}
```

### Processing a dispatch frame (asynchronous)

A received `dispatch` frame is handed to a worker goroutine so the read loop stays free for keepalive and further frames:

```
0. Send status frame { job_id, status: "printing" }
1. Fetch ciphertext from blob_url → []byte (RAM only)
2. RSA-OAEP decrypt encrypted_key using private key → rawAESKey []byte
3. Parse IV from first 12 bytes of ciphertext
4. AES-256-GCM decrypt ciphertext[12:] with rawAESKey and IV → plainPDF []byte
5. Write plainPDF to /dev/shm/automail-<job_id>.pdf (tmpfs)
6. exec.Command("lp", "-d", printerName, "/dev/shm/automail-<job_id>.pdf").Run()
7. os.Remove("/dev/shm/automail-<job_id>.pdf")
8. Zero: plainPDF, rawAESKey, ciphertextBlob byte slices
9. Send status frame { job_id, status: "delivered" } up the socket
10. Update slot occupancy; send a state frame reflecting the new occupancy
```

**Dev mode** (`DEV_MODE=true`): skip **only** step 6 (the physical `lp`/CUPS call). Steps 1–5 still run — including the real RSA/AES decryption — so the plaintext is written to the same tmpfs path (`/dev/shm/automail-<job_id>.pdf`) and unlinked (step 7) exactly as in production; dev mode just logs "dev: would print" in place of the `lp` invocation. Plaintext is never written to `/tmp` or any disk-backed path, even in dev, because it is now real decrypted content. (Superseded the earlier "write a dummy file to `/tmp`" Phase 3 stub, which pre-dated real decryption.)

---

## Keepalive and State Reporting

The persistent socket replaces the old periodic heartbeat POST. Two mechanisms keep the cloud server's view current:

1. **WebSocket ping/pong** every `HEARTBEAT_INTERVAL` seconds (default: 30) keeps the connection alive and proves liveness. A missed pong (or any read error) tears the connection down and triggers reconnect — the cloud server treats the closed socket as the printer going offline.
2. **`state` frames** are pushed whenever slot occupancy or status changes (and opportunistically alongside pings), so the cloud server's Redis cache stays fresh without a polling loop.

Slot occupancy is maintained in-process as a simple map, updated after each successful delivery and reflected in the next `state` frame.

---

## In-Memory Zeroing Pattern

```go
func zeroBytes(b []byte) {
    for i := range b {
        b[i] = 0
    }
}

// After printing is confirmed:
zeroBytes(plainPDF)
zeroBytes(rawAESKey)
zeroBytes(ciphertextBlob)
plainPDF = nil
rawAESKey = nil
ciphertextBlob = nil
runtime.GC()
```

Go's garbage collector does not guarantee immediate collection, but setting to nil removes the reference. For production, use `memguard` or OS-level locked memory pages. For this prototype, the zeroing + nil pattern is sufficient.

---

## Configuration (environment variables)

```
MAILBOX_ID               UUID of this mailbox unit (registered in cloud server DB)
PRINTER_NAME             CUPS printer name (e.g. "HP_LaserJet")
PRINTER_PRIVATE_KEY_PATH Path to encrypted RSA private key PEM
PRINTER_KEY_PASSPHRASE   Passphrase to decrypt the private key
MTLS_CA_CERT_PATH        Internal CA certificate
MTLS_CERT_PATH           This service's mTLS certificate
MTLS_KEY_PATH            This service's mTLS private key
CLOUD_SERVER_WS_URL      WebSocket URL to dial (e.g. wss://cloud-server:8443/internal/printer-link)
MINIO_URL                MinIO endpoint (used for health check only; blob_url is pre-signed)
HEARTBEAT_INTERVAL       Seconds between WebSocket ping/pong keepalives (default: 30)
RECONNECT_MAX_BACKOFF    Max seconds between reconnect attempts (default: 30)
DEV_MODE                 "true" to skip CUPS and write to /tmp instead
LISTEN_ADDR              Address for the local healthcheck endpoint only (default: :8444)
```

---

## Security Properties

- No plaintext ever written to persistent disk
- Private key passphrase zeroed after decryption
- tmpfs file unlinked before status callback is sent (unlink is not deferred)
- No document content in logs — only `job_id`, `status`, timestamps
- The printer accepts no inbound job connections; it only dials out, shrinking its attack surface to a single outbound socket plus a local healthcheck port
- The outbound WebSocket uses the same mTLS client certificate; the cloud server verifies it against the internal CA before upgrading the connection
- Dispatch, status, and state all travel over the one authenticated socket

---

## Project Structure

```
services/printer/
├── main.go            entry point, config loading, dial-out + healthcheck start
├── wsclient.go        persistent mTLS WebSocket: dial, register, read loop, reconnect/backoff
├── health.go          local GET /healthz for Docker container healthcheck
├── keepalive.go       ping/pong loop + state-frame reporting
├── print.go           decrypt + print + wipe logic (per dispatch frame)
├── crypto.go          RSA and AES helpers
└── mtls.go            mTLS client configuration (tls.Config for the dial)
```
