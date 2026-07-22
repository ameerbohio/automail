# Sender Portal

**Language**: TypeScript (Next.js)  
**Tag**: [SIMPLE]  
**Role**: Web interface for senders to encrypt and submit print jobs.

The portal's most interesting part is the in-browser encryption flow. The rest is standard Next.js — keep it minimal and functional.

---

## Pages

| Route | Purpose |
|---|---|
| `/login` | Email + password login form |
| `/` | Job submission form (protected) |
| `/jobs/:id` | Job status page with live SSE feed |

---

## Encryption Flow (the interesting part)

This runs entirely in the browser using `window.crypto.subtle`. No plaintext leaves the client.

```typescript
// lib/encrypt.ts

export async function encryptDocument(
  pdfBuffer: ArrayBuffer,
  printerPublicKeyPem: string
): Promise<{ ciphertext: ArrayBuffer; encryptedKey: ArrayBuffer }> {

  // 1. Generate a one-time AES-256-GCM key
  const aesKey = await crypto.subtle.generateKey(
    { name: 'AES-GCM', length: 256 },
    true,
    ['encrypt']
  )

  // 2. Encrypt the PDF
  const iv = crypto.getRandomValues(new Uint8Array(12))
  const encrypted = await crypto.subtle.encrypt(
    { name: 'AES-GCM', iv },
    aesKey,
    pdfBuffer
  )

  // Prepend IV to ciphertext: [12 bytes IV | ciphertext]
  const ciphertext = new Uint8Array(12 + encrypted.byteLength)
  ciphertext.set(iv, 0)
  ciphertext.set(new Uint8Array(encrypted), 12)

  // 3. Import printer's RSA public key
  const printerPubKey = await crypto.subtle.importKey(
    'spki',
    pemToDer(printerPublicKeyPem),
    { name: 'RSA-OAEP', hash: 'SHA-256' },
    false,
    ['wrapKey']
  )

  // 4. Wrap AES key with printer's RSA public key
  const encryptedKey = await crypto.subtle.wrapKey(
    'raw',
    aesKey,
    printerPubKey,
    { name: 'RSA-OAEP' }
  )

  return { ciphertext: ciphertext.buffer, encryptedKey }
}
```

---

## Job Submission Flow

```
1. User selects PDF file and recipient (building + unit)
2. Portal fetches mailbox public key: GET /mailboxes/:id/public-key
3. Portal calls encryptDocument(pdfBuffer, pubKeyPem)
4. Portal requests upload URL: POST /jobs/upload-url
5. Portal uploads ciphertext directly to MinIO (PUT to presigned URL)
6. Portal submits job: POST /jobs { encrypted_key, blob_ref, slot_id, page_count }
7. On success: redirect to /jobs/:id for live status
```

---

## SSE Status Display (`/jobs/:id`)

```typescript
// app/jobs/[id]/page.tsx (client component)
useEffect(() => {
  const es = new EventSource(`/api/jobs/${jobId}/stream`, {
    withCredentials: true
  })
  es.onmessage = (e) => {
    const { status } = JSON.parse(e.data)
    setStatus(status)
    if (status === 'delivered' || status === 'failed') es.close()
  }
  return () => es.close()
}, [jobId])
```

SSE connects through Next.js API route (which proxies to cloud server), or directly to the cloud server via Traefik depending on the routing setup.

---

## Authentication

- Login form posts to `/api/auth/login` (Next.js API route → cloud server)
- Access JWT stored in memory (not localStorage — not persistent across refresh, acceptable for prototype)
- Refresh token in httpOnly cookie — automatic via browser on all requests
- Next.js middleware checks for valid access token; redirects to `/login` if missing

---

## File Validation (client-side, UX only)

```typescript
const MAX_SIZE_BYTES = 20 * 1024 * 1024  // 20 MB

if (file.type !== 'application/pdf') {
  setError('Only PDF files are accepted')
  return
}
if (file.size > MAX_SIZE_BYTES) {
  setError('File must be under 20 MB')
  return
}
```

Server enforces these same constraints — client validation is for UX responsiveness only.

---

## Project Structure

```
services/portal/
├── app/
│   ├── login/page.tsx
│   ├── page.tsx              job submission form
│   └── jobs/[id]/page.tsx    job status + SSE
├── app/api/
│   ├── auth/
│   │   ├── login/route.ts
│   │   └── refresh/route.ts
│   ├── jobs/
│   │   ├── upload-url/route.ts
│   │   ├── route.ts          POST /api/jobs
│   │   └── [id]/
│   │       └── stream/route.ts  SSE proxy
│   └── printers/[id]/public-key/route.ts
├── lib/
│   ├── encrypt.ts            Web Crypto API encrypt flow
│   └── api.ts                fetch helpers with JWT header
├── middleware.ts             auth check, redirect to /login
├── package.json
└── tsconfig.json
```

---

## UI

No component library and no web fonts — one hand-written stylesheet (`app/globals.css`) plus a hand-authored inline SVG set (`app/icons.tsx`). Zero extra dependencies, and the portal renders identically with no egress, which the demo/printer boxes need.

**Design language — "Paper & Ink".** Physical-mail cues rather than dashboard chrome: warm envelope-stock paper instead of flat grey, a par-avion chevron band across the top of every page, a perforated-stamp logo, a franking postmark struck over delivered jobs, and a serif display face over a system sans (correspondence register, not app register).

- **Tokens.** Colour, type, radius, shadow and easing are CSS custom properties with a `prefers-color-scheme: dark` override. Everything downstream reads tokens, so dark mode costs no second asset.
- **Motion.** Small and purposeful: staggered entrance, a pulsing ring on the live stop, the postmark landing like a rubber stamp. All of it is disabled under `prefers-reduced-motion: reduce` — progress is encoded in colour and position, never in the animation alone.
- **Responsive.** One markup path. The journey tracker rotates from a horizontal route to a vertical rail under 560px in CSS; tables become stacked cards under 640px via `data-label` on each cell, so the markup stays a real `<table>`.

**Job journey tracker** (`app/journey.tsx`, shared by `/track` and `/jobs/:id`). The five ladder statuses are drawn as postal stops with a route line the document travels along, driven straight off the SSE frames. Each stop carries the time it was first seen, and a caption explains what happens to the bytes at the current stop — the tracker doubles as an explanation of the security model. Progress animates with a `scaleX`/`scaleY` transform rather than an animated width, so it is composited and cannot be caught mid-relayout.

The flow must stay observable: status transitions visible, error states clear.
