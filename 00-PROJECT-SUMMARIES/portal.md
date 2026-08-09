# Full-stack: the sender portal — at a glance

*Deep versions: [browser E2EE](../docs/study/18-web-crypto-e2ee-portal.md), [accounts & auth](../docs/study/19-sender-accounts-auth.md), [admin RBAC](../docs/study/20-ops-dashboard-rbac.md), [SSE vs WebSocket](../docs/study/17-sse-vs-websocket-redis-fanout.md).*

A Next.js (App Router) + TypeScript application where the front end is not a view layer over an
API — **it is where the cryptography happens.** The plaintext PDF and the AES key never leave the
browser.

## What it does

| Feature | The interesting part |
|---|---|
| **Encrypt and send** | Web Crypto: AES-256-GCM for the document, RSA-OAEP to wrap the key to the recipient mailbox's public key. The ciphertext goes **straight to object storage** via a pre-signed URL — it never transits the app's own server. |
| **Send with no account** | A one-time guest token, shown exactly once, hashed server-side. Track a job with a link and no signup. |
| **Accounts** | Register/login, RS256 JWTs, refresh token in an **HttpOnly** cookie, middleware gating account pages while the guest flow stays fully anonymous. |
| **Live status** | The job page subscribes over Server-Sent Events and renders `submitted → dispatching → printing → delivered` as it happens — no polling. SSE rather than WebSocket because the traffic is strictly one-way. |
| **Admin dashboard** | Role-gated queue counts, job table with filters, per-mailbox status. Enforced server-side, not by hiding UI. |

## The architectural decision worth defending

Next.js API routes are the obvious place a zero-knowledge design springs a leak: it's *your* server,
so it's tempting to do work there. Here they are **deliberately thin proxies** — they forward
ciphertext and metadata and nothing else. The trust boundary is the browser, not the portal's
server, and that is asserted by tests rather than left as an intention.

## Tested like a product, not a demo

- **Vitest units** on the encryption module (chunking, IV handling, malformed input) and on the
  proxy routes (correct forwarding, no auth leakage, guest-vs-authenticated path selection), with a
  coverage floor that ratchets — currently the highest in the repo at **70.9 %**.
- **Playwright** drives three real journeys against the running stack: guest send-and-track,
  account register-submit-history, and admin visibility with a non-admin JWT refused. One assertion
  intercepts the upload body and asserts it is **ciphertext**.
- Those browser tests earned their keep by catching two Content-Security-Policy bugs that `curl`
  structurally cannot see — a missing `style-src` (every React inline style blocked) and a missing
  `img-src` for `data:` URIs.

## What this is not

Design system rather than a design *language* — the UI is deliberately plain. Live status is proven
end-to-end in the tested configuration; relaying it through a reverse-proxy edge is a known open
issue, documented and guarded by a pending test rather than quietly left broken. Accessibility has
not been formally audited.

## Where to look

[`services/portal/lib/encrypt.ts`](../services/portal/lib/encrypt.ts) ·
[`services/portal/app/`](../services/portal/app/) (`track`, `jobs`, `history`, `admin`) ·
[`services/portal/lib/auth.tsx`](../services/portal/lib/auth.tsx) ·
[`services/portal/e2e/`](../services/portal/e2e/) ·
[`plans/06-sender-portal.md`](../plans/06-sender-portal.md)
