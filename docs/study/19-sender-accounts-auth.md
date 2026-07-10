# 19 — Sender accounts: open registration + the token model

**Phase 8.** Optional accounts layered on top of the guest flow. A resident can
register, send mail while logged in, and see their history — without ever
holding a per-job guest token. The guest flow keeps working untouched; accounts
are pure addition.

## Why the account flow exists next to guests

Phases 5–7 gave guests a working end-to-end flow, but tracking depends on a
one-time `guest_token` with no recovery path. An account replaces that with a
durable identity: jobs are stored with `sender_id`, and `GET /jobs` returns the
caller's whole history. Same submit pipeline, same E2EE — the only difference is
*who the job is attributed to* and *how you track it later*.

## Open registration, and the pgcrypto uniqueness problem

Registration is **open self-service** (`POST /auth/register`): email + password,
bcrypt hash, `role` forced to `sender`, and **auto-login** on success (it issues
the same token pair `Login` does) so signup is one step. No invite, no email
verification — least friction for a consumer product.

The interesting constraint: **you cannot put a `UNIQUE` index on the email.**
`senders.email_enc` is `pgp_sym_encrypt`'d PII, and pgcrypto's symmetric
encryption is *non-deterministic* — the same email encrypts to different bytes
every time (random IV). So two rows for the same email have different
ciphertext; a unique index sees them as distinct, and you can't look one up by
equality on the ciphertext either. The handler therefore pre-checks with
`GetSenderByEmail`, which **decrypts every row and compares** (a full scan,
acceptable at prototype scale). This also means the dup check has a narrow race:
two concurrent signups with the same email can both pass and insert. The
production fix is a **blind index** — store `HMAC(email)` in a separate,
deterministic, indexed column and enforce uniqueness on that, so you get O(log n)
lookups and a real constraint without ever storing plaintext. Worth naming in an
interview: it's the standard "searchable encryption" pattern.

## The token model: memory + HttpOnly cookie

Two tokens, deliberately different storage (plans/02, plans/06):

| Token | Lifetime | Stored | Why there |
|---|---|---|---|
| Access (RS256 JWT) | 15 min | JS memory only | Short-lived; memory (not localStorage) shrinks the XSS window — a script can't read it after the tab closes and it's never persisted. |
| Refresh (opaque) | 7 days | `HttpOnly` cookie | Long-lived; `HttpOnly` means JS *cannot* read it at all, so XSS can't exfiltrate it. Only the server (via the browser) ever sees it. |

**Bootstrap on load:** because the access token lives in memory, a page reload
loses it. On mount the `AuthProvider` calls `POST /api/auth/refresh`, which
trades the HttpOnly refresh cookie for a fresh access token — so the session
survives reloads without ever persisting the JWT where a script could read it.

**Rotation:** the refresh token is single-use. Every `/auth/refresh` revokes the
old one and sets a new cookie. Tradeoff to know: with bootstrap-on-load, two
tabs opening at once can race — the first rotates the token, the second presents
the now-stale one and gets logged out. Fine for the prototype; a production
build would use a short grace window or a dedicated session record.

## Two proxy details that make the cookie work

The portal's Next.js API routes are same-origin proxies to the cloud server.
Two things are easy to get wrong:

1. **Cookie-path rewrite.** The cloud sets the refresh cookie with
   `Path=/auth/refresh`. But the browser only talks to the *portal's* routes
   (`/api/auth/refresh`), so that path would never match and the cookie would
   never be sent back. The proxy rewrites `Path=/auth/refresh` → `Path=/` on the
   relayed `Set-Cookie`, so the browser returns it to `/api/auth/refresh` and
   the account-page middleware can see it too. (`getSetCookie()` is used to read
   multiple `Set-Cookie` headers correctly.)
2. **Authenticated SSE.** `EventSource` cannot set an `Authorization` header —
   the same limitation that pushes the guest token into the query string. So the
   authenticated `/jobs/:id` page passes its in-memory access token as
   `?access=<jwt>`; the proxy converts it to a `Bearer` header for the cloud,
   which runs the JWT ownership check. The token is short-lived and only appears
   at connect time. (A stricter design would mint a one-time stream ticket
   instead of putting the JWT in a URL.)

## Middleware gates account pages *only*

`middleware.ts` matches just `/history` and `/jobs/*` and redirects to `/login`
when the refresh cookie is absent. It's a **lightweight UX gate** — presence of
a cookie, not a validity check — so a guest doesn't see an account page flash
before the client redirects. Real enforcement is still the cloud API returning
`401`/`404`, which the pages also handle. Crucially, `/` and `/track` are *not*
matched, so the guest flow stays fully open — the Phase 8 requirement that
"guest flow still works unauthenticated."

## Interview soundbite

> Accounts are optional and additive: same E2EE submit pipeline, but jobs get a
> `sender_id` and tracking moves from a one-time guest token to a durable
> history. The token model is access-in-memory + refresh-in-HttpOnly-cookie,
> bootstrapped on load so reloads survive without persisting the JWT. The
> sharp edges are all consequences of earlier design choices: email uniqueness
> can't be a DB constraint because the column is non-deterministically
> encrypted (blind index is the fix), and both the SSE auth and the cookie path
> need special handling because `EventSource` can't send headers and the proxy
> sits on a different path than the cloud server.
