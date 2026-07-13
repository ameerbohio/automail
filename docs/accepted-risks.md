# Accepted Risks

Security findings that have been reviewed and **deliberately accepted** rather
than fixed, with the reasoning and the conditions that should trigger a re-review.
The `docs/release-checklist.md` (Goal T11) references this file: accepting a risk
here is what lets a release proceed with the finding open.

Scanners are wired in Goal T3 (`make scan`). `npm audit` is **informational, not a
blocking CI gate**, precisely because of the accepted items below — it flags by
package *version*, not by whether the vulnerable code path is used.

---

## AR-1 — Residual Next.js advisories (portal), pending a Next 16 upgrade

**Status:** accepted · **Reviewed:** 2026-07-13 · **Severity as reported:** 1 high, 1 moderate

After upgrading `next` 14.2.5 → 14.2.35 (which resolved the CRITICAL middleware
authorization-bypass, GHSA-f82v-jwr5-mffw), `npm audit --omit=dev` still reports:

| Advisory | Requires (to be exploitable) | Portal usage | Verdict |
|---|---|---|---|
| Image Optimizer DoS (`remotePatterns`); unbounded `next/image` disk cache | `next/image` | No `next/image` anywhere | Not applicable |
| HTTP request smuggling in rewrites | `next.config` rewrites | Config has only `output: "standalone"` — no rewrites | Not applicable |
| RSC deserialization DoS; DoS with Server Components | Server Actions / RSC payloads | No `"use server"`; client components + API-route proxies | Not applicable |
| postcss XSS via CSS stringify (moderate) | processing *untrusted* CSS | Build-time only, on developer-authored CSS | Not exploitable |

**Why accepted:**
1. **The vulnerable features are not used** — verified by grep (`next/image`,
   rewrites, `use server`, postcss config all absent). The advisories exist in the
   `next` package generally; this app does not reach the affected code paths.
2. **Class is DoS/availability**, not authorization or data disclosure — and this
   is a **low-traffic, single-tenant, self-hosted** deployment behind Traefik, not
   a public multi-tenant service. (The one authorization-class finding — the
   middleware auth bypass — was *not* accepted; it was fixed.)
3. The only upstream fix is **Next 16**, a major breaking upgrade (async request
   APIs, etc.) requiring real migration and full app-level verification —
   disproportionate to the residual risk today.

**Re-review triggers (do the Next 16 migration if any becomes true):**
- The portal adopts `next/image`, `next.config` rewrites/redirects, or Server
  Actions (`"use server"`).
- The portal is exposed to untrusted/multi-tenant traffic or a public internet
  surface beyond the intended personal deployment.
- A new advisory against `next@14.2.x` is **authorization- or
  disclosure-class** (not DoS) — treat like the original critical and fix now.

**Revisit by:** next public-exposure change, or the next dependency-audit review.
