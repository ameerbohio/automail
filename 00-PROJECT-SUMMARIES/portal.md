# Full-stack: the sender portal — at a glance

*Deep versions: [browser E2EE](../docs/study/18-web-crypto-e2ee-portal.md), [accounts & auth](../docs/study/19-sender-accounts-auth.md), [admin RBAC](../docs/study/20-ops-dashboard-rbac.md), [SSE vs WebSocket](../docs/study/17-sse-vs-websocket-redis-fanout.md).*

A Next.js (App Router) + TypeScript application where the front end is not a view layer over an
API — **it is where the cryptography happens.** The plaintext PDF and the AES key never leave the
browser.

Every row and bullet below carries a **see** pointer to the file that proves it.

## What it does

| Feature | The interesting part | See |
|---|---|---|
| **Encrypt and send** | Web Crypto: AES-256-GCM for the document, RSA-OAEP to wrap the key to the recipient mailbox's public key. The ciphertext goes **straight to object storage** via a pre-signed URL — it never transits the app's own server. | [`encryptDocument`](../services/portal/lib/encrypt.ts#L39) · [the send page](../services/portal/app/page.tsx) · [the URL-minting route](../services/portal/app/api/jobs/upload-url/) · [the server-side presigner](../services/cloud/minioclient/) |
| **Send with no account** | A one-time guest token, shown exactly once, hashed server-side. Track a job with a link and no signup. | [`newOpaqueToken` / `hashToken` — only the hash is stored](../services/cloud/handlers/tokens.go#L35) · [the `/track` page](../services/portal/app/track/) · [the explainer](../docs/study/09-guest-token-vs-jwt.md) |
| **Accounts** | Register/login, RS256 JWTs, refresh token in an **HttpOnly** cookie, middleware gating account pages while the guest flow stays fully anonymous. | [`Register` / `Login` / `Refresh`](../services/cloud/handlers/auth.go#L204) · [the client-side session](../services/portal/lib/auth.tsx) · [the route matcher — exactly which paths are gated](../services/portal/middleware.ts#L25) · [server-side tests](../services/cloud/account_test.go) |
| **Live status** | The job page subscribes over Server-Sent Events and renders `submitted → dispatching → printing → delivered` as it happens — no polling. SSE rather than WebSocket because the traffic is strictly one-way. | [the SSE relay route](../services/portal/app/api/jobs/%5Bid%5D/stream/route.ts) · [the job page that consumes it](../services/portal/app/jobs/) · [the cloud-side fan-out](../services/cloud/stream_test.go) · [why SSE and not WebSocket](../docs/study/17-sse-vs-websocket-redis-fanout.md) |
| **Admin dashboard** | Role-gated queue counts, job table with filters, per-mailbox status. Enforced server-side, not by hiding UI. | [`app/admin/`](../services/portal/app/admin/) · [the handlers](../services/cloud/handlers/admin.go#L49) · [`requireAdmin`, the actual gate](../services/cloud/middleware.go#L83) · [the test that a non-admin JWT is refused](../services/cloud/admin_test.go) |

## The architectural decision worth defending

Next.js API routes are the obvious place a zero-knowledge design springs a leak: it's *your* server,
so it's tempting to do work there. Here they are **deliberately thin proxies** — they forward
ciphertext and metadata and nothing else. The trust boundary is the browser, not the portal's
server, and that is asserted by tests rather than left as an intention.

**see** [`lib/proxy.ts` — the whole forwarding surface, ~60 lines](../services/portal/lib/proxy.ts) ·
[`forwardAuth`, which forwards headers rather than minting anything](../services/portal/lib/proxy.ts#L12) ·
[`proxy.test.ts` — correct forwarding, no auth leakage, guest-vs-authenticated path selection](../services/portal/lib/proxy.test.ts) ·
[the routes themselves](../services/portal/app/api/)

## Tested like a product, not a demo

- **Vitest units** on the encryption module (chunking, IV handling, malformed input) and on the
  proxy routes, with a coverage floor that ratchets — currently the highest in the repo at **70.9 %**.
  **see** [`encrypt.test.ts`](../services/portal/lib/encrypt.test.ts) ·
  [`proxy.test.ts`](../services/portal/lib/proxy.test.ts) ·
  [`api.test.ts`](../services/portal/lib/api.test.ts) ·
  [the floor](../scripts/coverage.floors) · [the gate](../scripts/coverage-portal.sh)
- **Playwright** drives three real journeys against the running stack. One assertion intercepts the
  upload body and asserts it is **ciphertext**.
  **see** [guest send-and-track](../services/portal/tests/browser/guest.spec.ts) ·
  [account register-submit-history](../services/portal/tests/browser/account.spec.ts) ·
  [admin visibility, with a non-admin JWT refused](../services/portal/tests/browser/admin.spec.ts) ·
  [the ciphertext assertion itself](../services/portal/tests/browser/guest.spec.ts#L22) ·
  [`make test-e2e`](../Makefile#L92)
- Those browser tests earned their keep by catching two Content-Security-Policy bugs that `curl`
  structurally cannot see — a missing `style-src` (every React inline style blocked) and a missing
  `img-src` for `data:` URIs.
  **see** [the fixed CSP, with both causes written down where they were fixed](../infra/k8s/base/ingress/middlewares.yaml#L32) ·
  [the same CSP on the Compose edge, kept in step](../infra/traefik/)

## What this is not

| Limit | See |
|---|---|
| A design *system*, not a design language — the UI is deliberately plain | [`globals.css`](../services/portal/app/globals.css) · [the explainer](../docs/study/21-portal-design-system.md) |
| Live status is proven in the tested configuration; relaying it through a reverse-proxy edge is a known open issue, guarded by a pending test rather than quietly left broken | [the `test.fixme` that names it on every run](../services/portal/tests/ingress/ingress.spec.ts) · [the write-up and the decision it needs](../docs/study/00-interview-pending-questions.md) |
| Accessibility has not been formally audited | [the pages, unaudited](../services/portal/app/) |
| The spec this was built against | [`plans/06-sender-portal.md`](../plans/06-sender-portal.md) |
