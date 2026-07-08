# 18 — Browser E2EE with Web Crypto (the guest portal)

**Phase 7.** The sender portal encrypts the PDF *in the browser* before anything
leaves the tab. This is the encrypt half of the hybrid scheme whose decrypt half
lives in the printer (see [16-hybrid-encryption.md](16-hybrid-encryption.md)).
The whole point: the cloud server is zero-knowledge — it only ever sees
ciphertext and an RSA-wrapped key it cannot open.

## The flow, end to end

```
browser                          cloud server            MinIO        printer
  |-- GET /recipients?q= ----------->|  (masked names)
  |<- [{recipient_id, ...}] ---------|
  |-- GET /recipients/:id/public-key>|  (resolves recipient -> printer key)
  |<- { public_key_pem } ------------|
  |  encryptDocument() in Web Crypto  (AES-GCM + RSA-OAEP)   <-- nothing leaves yet
  |-- POST /jobs/upload-url --------->|
  |<- { upload_url, blob_ref } -------|
  |-- PUT ciphertext ------------------------------------->| (direct, presigned)
  |-- POST /jobs {encrypted_key,...} >|  (stores opaque ciphertext + wrapped key)
  |<- { job_id, guest_token } --------|
  |-- GET /jobs/:id/stream?token= --->|  SSE: submitted->...->delivered
                                       |--- dispatch ------------------->| decrypts,
                                                                          prints, wipes
```

Two things never touch the network in plaintext: the **PDF bytes** (only the
AES-GCM ciphertext is uploaded, straight to MinIO) and the **AES key** (only its
RSA-OAEP wrap, as `encrypted_key`). The server literally cannot decrypt either —
it holds no private key. That's the property that makes it *zero-knowledge*, not
just *encrypted-at-rest*.

## Why hybrid (AES + RSA) instead of just one?

- **RSA can't encrypt bulk data.** RSA-4096 OAEP can wrap at most ~446 bytes. A
  PDF is megabytes. So we encrypt the document with a fast symmetric cipher
  (AES-256-GCM) and use RSA only to wrap the 32-byte AES key.
- **AES alone can't solve key distribution.** The sender and printer share no
  secret. RSA lets the sender encrypt *to* the printer's public key with no
  prior handshake. Hybrid gets both: RSA's keyless-to-the-sender distribution +
  AES's bulk speed.

## The wire contract (must match the printer byte-for-byte)

`lib/encrypt.ts` is written against exactly what `services/printer/crypto.go`
decrypts. If either side drifts, delivery silently fails:

| Field | Portal (Web Crypto) | Printer (Go) |
|---|---|---|
| Blob layout | `[12-byte IV \|\| GCM ciphertext+tag]` | reads IV = first 12 bytes |
| AAD | none | none |
| AES | AES-256-GCM, 96-bit random IV, 128-bit tag | AES-256-GCM |
| Key wrap | RSA-OAEP, SHA-256 + MGF1(SHA-256), empty label | RSA-OAEP, SHA-256 |
| `encrypted_key` on the wire | standard base64 (`btoa`) | `base64.StdEncoding.DecodeString` |

Two easy-to-miss details:

- **Web Crypto appends the GCM tag** to the ciphertext automatically, and takes
  the IV as a separate parameter — so *we* prepend the IV by hand to build the
  `[IV || ct+tag]` layout the printer expects. The tag is not something we
  manage explicitly on either side.
- **Web Crypto fixes MGF1's hash equal to the OAEP hash.** We pass `hash:
  'SHA-256'` at import; MGF1 is therefore SHA-256 too, with an empty label —
  which is what Go's `rsa.DecryptOAEP(sha256, ...)` with a nil label expects.

## `generateKey(..., extractable = true)` — isn't that a leak?

The AES key is generated `extractable: true`. That sounds wrong for a secret,
but "extractable" only means *the same tab's JS* could export it via
`crypto.subtle.exportKey`. We never call that. We need it extractable so
`wrapKey` can serialize-and-encrypt it under RSA in one shot. The raw key bytes
never become a JS value and never leave the browser. If it were
`extractable: false`, `wrapKey` would fail.

## Why proxy through Next.js API routes instead of calling the cloud directly?

The browser talks only to same-origin `/api/*` routes (`app/api/...`), which
relay to the cloud server server-side. Reasons:

1. **CORS.** The portal is `automail.local`; the API is `api.automail.local`. A
   cross-origin `EventSource`/`fetch` would need CORS headers the cloud server
   doesn't emit. Same-origin sidesteps it entirely.
2. **The cloud hostname stays server-side** (`CLOUD_API_URL`), never shipped to
   the client bundle.
3. The proxies are **thin**: they pass status and body through verbatim and
   never parse or log the body — critical so `encrypted_key` stays opaque.

The one exception is the **blob PUT**, which goes browser → MinIO directly via
the presigned URL. Routing a multi-MB upload through Next would defeat the
point of presigning (see [08-presigned-urls-direct-upload.md](08-presigned-urls-direct-upload.md)).

## Why the guest token rides in the SSE query string

`EventSource` cannot set request headers — no `Authorization: Bearer`. So the
guest tracks a job with `GET /jobs/:id/stream?token=<guest_token>`, and the
cloud server SHA-256s it and compares to `jobs.guest_token_hash`
(see [09-guest-token-vs-jwt.md](09-guest-token-vs-jwt.md)). The token is shown
to the sender exactly once at submission; there is no recovery path if it's
lost, because the server only stores its hash — a stolen DB dump yields no
usable token.

## Interview soundbite

> The portal does hybrid encryption in the browser with Web Crypto: a one-time
> AES-256-GCM key encrypts the PDF, and RSA-OAEP wraps that key to the printer's
> public key. Only ciphertext and the wrapped key ever leave the tab, so the
> cloud server — which holds no private key — is genuinely zero-knowledge, not
> just encrypted-at-rest. The byte layout and OAEP parameters are pinned to
> exactly what the Go printer decrypts; the interesting bugs are all
> contract-drift bugs (IV placement, MGF1 hash, base64 flavor).
