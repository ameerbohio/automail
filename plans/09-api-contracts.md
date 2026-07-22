# API Contracts

All external endpoints are served via Traefik (HTTPS, TLS 1.3). Internal endpoints are mTLS only and unreachable from outside the Docker network.

---

## Response Header (all external endpoints)

```
X-Automail-Node: <NODE_ID>
```

Every response from the cloud server names the node that served it — the same `NODE_ID` (defaulting to `$HOSTNAME`) that identifies the node as a Redis Streams consumer. Set by a middleware wrapping the whole mux, so it is present on success, on errors, and on the SSE stream's initial response.

Metadata only: a consumer name, never a secret, and never anything derived from the document. The portal's `/api/*` proxies forward this one header and the sender portal shows which node handled a submission — the point being that the backend is N stateless nodes and no node holds state that would pin a session. It does disclose topology; see `plans/13-v2-roadmap.md` "Richer request-path observability" for why a non-demo deployment would gate it behind a flag or emit an opaque per-boot id.

---

## External Endpoints (cloud server, authenticated unless noted)

### `GET /healthz`

**Auth**: None  
**Response**: `200 OK` if Postgres and Redis reachable; `503` otherwise.

```json
{ "status": "ok" }
```

---

### `POST /auth/login`

**Auth**: None  
**Body**:
```json
{ "email": "sender@example.com", "password": "..." }
```
**Response `200`**:
```json
{ "access_token": "<jwt>", "expires_in": 900 }
```
Sets `Set-Cookie: refresh_token=<token>; HttpOnly; Secure; SameSite=Strict; Path=/auth/refresh`

**Response `401`**: invalid credentials

---

### `POST /auth/register`

**Auth**: None (rate-limited at Traefik — account creation is abuse-sensitive)

Registration is **open self-service**: any resident can create a sender account
to send mail and see their own history, with the least possible friction. There
is no invite, no admin approval, and no email-verification step in the prototype
— sign up and you are immediately logged in. The `senders` table
(`08-data-models.md`) already has `email`, `password_hash`, `role`.

**Body**:
```json
{ "email": "sender@example.com", "password": "..." }
```

**Validation** (server-side, authoritative):
- `email` must be a syntactically valid address and unique in `senders`.
- `password` minimum 8 characters. Hashed with bcrypt (same cost as `Login`);
  the plaintext password is never stored or logged.
- New rows get `role = 'sender'` — the admin role is never self-assignable
  through this endpoint.

**Effect**: inserts a `senders` row, then **auto-logs-in** — it issues the same
token pair `Login` does, so the portal lands the user straight in the
authenticated flow with no second round-trip.

**Response `201`**:
```json
{ "access_token": "<jwt>", "expires_in": 900 }
```
Sets `Set-Cookie: refresh_token=<token>; HttpOnly; Secure; SameSite=Strict; Path=/auth/refresh`

**Response `409`**: email already registered (`code: EMAIL_TAKEN`)
**Response `422`**: invalid email or password too weak (`code: VALIDATION`)

The guest flow is unaffected — sending mail never requires an account; an
account only adds persistent, tokenless job history.

---

### `POST /auth/refresh`

**Auth**: Refresh token cookie  
**Response `200`**: new access token + rotated refresh cookie  
**Response `401`**: expired or revoked token

---

### `POST /auth/logout`

**Auth**: Bearer JWT  
**Effect**: Revokes refresh token in DB; clears cookie.  
**Response**: `204 No Content`

---

### `GET /recipients`

**Auth**: None (rate-limited at Traefik)  
**Query**: `q=<name or building address>` (minimum 2 characters)  
**Response `200`**:
```json
[
  {
    "recipient_id": "uuid",
    "display_name": "J. Smith",
    "building_address": "123 Main St"
  }
]
```
Full name is never returned; the server masks it to first initial + last name to limit PII exposure. Slot and printer identifiers are not included.

---

### `GET /recipients/:id/public-key`

**Auth**: None (rate-limited at Traefik)  
**Response `200`**:
```json
{ "recipient_id": "uuid", "public_key_pem": "-----BEGIN PUBLIC KEY-----\n..." }
```
The server resolves the recipient's printer internally. The sender never sees the printer ID or slot ID.  
**Response `404`**: recipient not found or slot unassigned

---

### `POST /jobs/upload-url`

**Auth**: Bearer JWT (optional — omit for guest)  
**Body**:
```json
{ "recipient_id": "uuid", "filename": "doc.pdf.enc" }
```
**Response `200`**:
```json
{
  "upload_url": "https://minio.internal/automail/blobs/<uuid>?X-Amz-Signature=...",
  "blob_ref": "blobs/<uuid>",
  "expires_in": 900
}
```
The browser PUT's the encrypted blob directly to `upload_url`. The cloud server never receives the blob.

---

### `POST /jobs`

**Auth**: Bearer JWT (optional — omit for guest)  
**Body**:
```json
{
  "encrypted_key": "<base64 RSA-OAEP ciphertext>",
  "blob_ref": "blobs/<uuid>",
  "recipient_id": "uuid",
  "page_count": 3
}
```
The server resolves `recipient_id` to `slot_id` and `mailbox_id` before storing the job. The sender never supplies or observes either.

If no Bearer JWT is present the job is stored with `sender_id = NULL`. A one-time `guest_token` is generated, its SHA-256 hash stored in `jobs.guest_token_hash`, and the raw token returned in the response. If the token is lost, the job cannot be tracked — there is no recovery path.

**Response `202`** — authenticated (immediate dispatch):
```json
{ "job_id": "uuid", "status": "dispatching" }
```
**Response `202`** — authenticated (queued):
```json
{ "job_id": "uuid", "status": "queued" }
```
**Response `202`** — guest (immediate dispatch):
```json
{ "job_id": "uuid", "status": "dispatching", "guest_token": "<one-time token>" }
```
**Response `202`** — guest (queued):
```json
{ "job_id": "uuid", "status": "queued", "guest_token": "<one-time token>" }
```
**Response `400`**: recipient not found, slot unassigned, or printer not registered  
**Response `422`**: blob_ref not found in MinIO (pre-check)

---

### `GET /jobs/:id/stream`

**Auth**: Bearer JWT (authenticated sender, must own the job) — or — `?token=<guest_token>` (guest, verified against `guest_token_hash`)  
**Protocol**: Server-Sent Events (`Content-Type: text/event-stream`)

Event format:
```
data: {"job_id":"uuid","status":"printing"}\n\n
data: {"job_id":"uuid","status":"delivered"}\n\n
```

Possible `status` values in order: `queued` → `dispatching` → `printing` → `delivered` | `failed`

Connection closes when status reaches `delivered` or `failed`.

Note for the Phase 5 implementer: the internal `job:<id>:status` Redis pub/sub payload (written by the printer-link hub, see `link.statusPayload`) deliberately omits `job_id` — the channel name already scopes it. The stream handler already knows `job_id` from the URL path, so it must add the field back in when forming the SSE `data:` line above.

---

### `GET /admin/summary`

**Auth**: Bearer JWT (`admin` role)

Aggregate figures for the `/admin` overview (`07-ops-dashboard.md`) — the
numbers the two list endpoints below cannot cheaply produce (queue depth would
be one call per status; "completed today" is a time-bounded count the paginated
job list can't express). Metadata only: pure counts, no job identifiers, no
ciphertext.

**Response `200`**:
```json
{
  "status_counts": { "queued": 1, "dispatching": 0, "printing": 1, "delivered": 5, "failed": 0 },
  "queue_depth": 1,
  "completed_today": 3
}
```
`queue_depth` counts jobs awaiting a printer (`submitted` + `queued` +
`dispatching`). `completed_today` counts jobs `delivered` since 00:00 UTC.

---

### `GET /admin/jobs`

**Auth**: Bearer JWT (`admin` role)  
**Query params**: `status` (optional exact-status filter; empty = all), `page` (default 1), `per_page` (default 50, max 200)  
**Response `200`**:
```json
{
  "jobs": [
    {
      "job_id": "uuid",
      "slot_id": "uuid",
      "slot_number": 3,
      "status": "delivered",
      "page_count": 2,
      "created_at": "2026-05-23T10:00:00Z",
      "delivered_at": "2026-05-23T10:01:05Z"
    }
  ],
  "total": 142,
  "page": 1
}
```
No `encrypted_key` or `blob_ref` in response.

---

### `GET /admin/mailboxes`

**Auth**: Bearer JWT (`admin` role)  
**Response `200`**:
```json
{
  "mailboxes": [
    {
      "mailbox_id": "uuid",
      "building_address": "123 Main St",
      "status": "idle",
      "last_heartbeat_at": "2026-05-23T10:00:30Z",
      "slot_occupancy": {
        "slot-uuid-1": { "slot_number": 1, "current": 2, "max": 5 },
        "slot-uuid-2": { "slot_number": 2, "current": 0, "max": 5 }
      }
    }
  ]
}
```

---

## Internal Link (mTLS only, Docker bridge network)

There is no inbound HTTP endpoint on the printer. All printer ↔ cloud traffic flows over a single persistent mTLS WebSocket that the printer **dials out** to the cloud server and holds open. The cloud server verifies the printer's client certificate against the internal CA during the TLS handshake, then upgrades the connection. See [04-printer-microservice.md](04-printer-microservice.md) and [05-cloud-server.md](05-cloud-server.md).

### `GET /internal/printer-link`  (WebSocket upgrade)

**Dialer**: Printer microservice → cloud server (`wss://cloud-server:8443/internal/printer-link`)  
**Auth**: mTLS client certificate (internal CA), verified before upgrade  
**Response**: `101 Switching Protocols` on success; `403` if the client cert is not trusted.

After the upgrade, both directions exchange JSON text frames discriminated by `type`. WebSocket ping/pong provides keepalive; a closed socket is the printer's offline signal.

**Printer → cloud frames**

```json
{ "type": "register", "mailbox_id": "uuid",
  "slot_occupancy": { "slot-uuid-1": { "current": 2, "max": 5 } } }
```
Sent once immediately after connect to register presence.

```json
{ "type": "state", "status": "idle",
  "slot_occupancy": { "slot-uuid-1": { "current": 2, "max": 5 },
                      "slot-uuid-2": { "current": 0, "max": 5 } } }
```
Sent on slot-occupancy/status change (replaces the old heartbeat POST). `status` is `idle | printing`.

```json
{ "type": "status", "job_id": "uuid",
  "status": "printing",
  "error": "optional error message if failed" }
```
Job lifecycle update; `status` is `printing | delivered | failed` (replaces the old status POST).

**Cloud → printer frames**

```json
{ "type": "dispatch", "job_id": "uuid",
  "encrypted_key": "<base64>",
  "blob_url": "<presigned MinIO read URL, 5 min TTL>" }
```
Pushed when the cloud server dispatches a job to this printer (replaces the old dispatch POST). The cloud server only dispatches to printers whose socket is live, so there is no "printer busy" response — capacity is checked from the Redis state cache before the frame is sent.

---

## Error Format (all endpoints)

```json
{
  "error": "human-readable message",
  "code": "MACHINE_READABLE_CODE"
}
```

Common codes: `UNAUTHORIZED`, `FORBIDDEN`, `NOT_FOUND`, `RECIPIENT_NOT_FOUND`, `SLOT_UNASSIGNED`, `PRINTER_UNAVAILABLE`, `SLOT_FULL`, `INVALID_BLOB_REF`, `GUEST_TOKEN_INVALID`, `EMAIL_TAKEN`, `VALIDATION`
