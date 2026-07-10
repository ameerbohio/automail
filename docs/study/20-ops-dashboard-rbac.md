# Ops Dashboard: role-gated, metadata-only, cache-derived state

**Phase 9.** `services/cloud/handlers/admin.go`, `services/cloud/middleware.go`
(`requireAdmin`), `services/portal/app/admin/*`. Contract: `plans/09` (`GET
/admin/summary`, `/admin/jobs`, `/admin/mailboxes`), design: `plans/07`.

## What it is

A read-only operator view over the same system the senders drive: how many jobs
are queued, how many delivered today, and the live status + slot occupancy of
each mailbox. Three cloud endpoints behind an `admin`-role JWT check, three
Next.js pages that poll them every 15s.

## Three things worth being able to explain

### 1. Authorization is a role check layered on authentication

`requireAuth` answers *who are you* (valid RS256 access token → sender identity
in the request context). `requireAdmin` adds *are you allowed*: same token
parse, plus `claims.Role != "admin"` → **403 FORBIDDEN**, distinct from the
**401 UNAUTHORIZED** you get with no/expired token. The distinction is the
interview point: 401 means "I don't know who you are, authenticate"; 403 means
"I know exactly who you are, and you're not permitted." Returning 401 for an
authenticated-but-unentitled user would tell the client to go refresh a token
that was never the problem.

The admin role is **never self-assignable**: `Register` hard-codes
`role = 'sender'` ([[19-sender-accounts-auth]]), so an admin row exists only if
seeded directly in the database. Privilege escalation has no code path — there
is no endpoint that writes `role`.

### 2. Metadata only — the zero-knowledge boundary holds for admins too

An operator is still not a trusted party for *content*. Every admin query
(`AdminListJobs`, `AdminJobStatusCounts`, …) deliberately omits `encrypted_key`
and `blob_ref` — the dashboard can show that job `abc123` is `delivered` with 2
pages, but there is no column, endpoint, or UI element that could surface the
ciphertext key or the blob pointer. This is the same discipline as the sender
history ([[19-sender-accounts-auth]]) and the SSE stream
([[17-sse-vs-websocket-redis-fanout]]): admin is a *broader* view, not a
*deeper* one. The test asserts the JSON body contains neither string.

### 3. Live status is derived from the cache, not the database row

`mailboxes.status` in Postgres is only ever the `'offline'` default — the
printer-link hub writes liveness to **Redis** (`mailbox:<id>:state`, 90s TTL),
not back to the row ([[11-dispatch-fan-in-printer-link]]). So the dashboard's
"is this printer up?" cannot come from the DB. `AdminMailboxes` reads the DB for
the *stable* facts (address, configured slots and their capacity) and overlays
the *live* facts from Redis:

- **present** cache entry → the printer's own reported status (`idle`/`printing`)
  and current per-slot occupancy;
- **absent** entry (`LookupPrinterState` returns `found=false`) → `offline`,
  with slot capacity still shown from the DB and current occupancy 0.

That `found` boolean is the whole reason `LookupPrinterState` exists separately
from `GetPrinterState`: the dispatch path wants a missing key to *mean*
"empty-but-usable idle printer" (its Phase 2 default), but the dashboard must
distinguish "idle" from "offline" — conflating them would show a dead unit as
healthy. Same key, opposite desired default, so the raw lookup is exposed and
each caller picks its own interpretation. The offline signal being a **TTL
lapse** (not a closed socket) is what lets a frozen-but-connected printer still
read offline.

## Why an extra `/admin/summary` endpoint

The two list endpoints can't cheaply produce the overview's headline numbers.
"Jobs in queue" is a count across three statuses (one call per status if you
only had the paginated list), and "completed today" is a *time-bounded* count
the job list can't express at all. So `summary` is a small aggregate endpoint:
`GROUP BY status` plus a `delivered_at >= start-of-day-UTC` count. Pure numbers,
no identifiers — the cheapest thing that satisfies `plans/07`'s overview without
the frontend fanning out N filtered list calls.

## The honest caveats (likely follow-ups)

- **Two liveness signals, deliberately.** The *badge* (idle/printing/offline)
  is derived from the Redis cache's presence (`LookupPrinterState`) because the
  90s TTL is the authoritative "is it up right now" signal; the *timestamp*
  ("last heartbeat") comes from the DB `mailboxes.last_heartbeat_at`, the
  "durable mirror" `plans/08` calls for. The printer-link hub writes that mirror
  (`UpdateMailboxLiveness`) on every register/state frame — best-effort, nil-
  guarded, never fatal to the link. Why not read the timestamp from Redis too?
  The cache entry *vanishes* on TTL lapse, so it can't tell you *when* a now-
  offline printer was last seen; the durable row can. So: cache for "up now",
  row for "last seen" — each source used for what it can actually answer.
- **Polling, not SSE.** `plans/07` calls for a 15s `setInterval`. Operational
  visibility doesn't need per-event latency, and a dashboard left open
  shouldn't hold a socket per operator. Contrast the sender's per-job view,
  which *is* SSE ([[17-sse-vs-websocket-redis-fanout]]) — the transport choice
  follows the latency requirement.
- **Status-filter counts vs page window.** `GET /admin/jobs` returns `total`
  for the *filtered* set (a second `COUNT(*)` with the same predicate), not the
  page length, so pagination math is correct independent of which page you're
  on.
- **Frontend role gate is cosmetic.** The Next middleware only checks for a
  session cookie; the true admin gate is the cloud 403, which the pages render
  as "not authorized." The browser is never trusted to decide entitlement —
  it only decides what to *show* while the server decides what to *serve*.
