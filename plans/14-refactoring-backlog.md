# Refactoring Track — Duplication, Simplification, and a Health Rating

> **Scope banner.** This file is **not** specification. Nothing in it changes
> behaviour, adds a feature, or alters the security model — every goal is "the
> same system, expressed with less repetition". It is an independent third track
> alongside the phase track and Testing Track in [GOALS.md](../GOALS.md), with
> its own run prompt below. It is **not** gated by them and does not gate them.

Run it with its own recurring prompt:

> Read `plans/14-refactoring-backlog.md`. Find the first goal (R1, R2, …) whose
> Status is `pending`, execute it following the Process Rules, verify its
> Acceptance, mark it `done` (update its Status line **and** append a Status Log
> entry), then **stop** — one goal per run. Do not batch goals. Stop early and
> record why if Acceptance cannot be verified, or if the goal turns out to change
> behaviour rather than just restructure it.

---

## How this was measured

A read of every hand-written source file over ~11.5k lines (4.6k Go non-test,
4.8k Go test, 3.5k portal TS, ~1.5k shell), plus counted greps for repeated
constructs. Every count in this file is a real number from this tree, not an
estimate. Goals are ordered by **(repetition × blast radius) ÷ effort** — how
many places say the same thing, what breaks silently if one copy drifts, and how
much work the fix is.

The distinction that drives the ordering: *duplication that can drift into a bug*
outranks *duplication that is merely verbose*. Three copies of a Redis channel
name are dangerous. Thirty-one `WriteError(500, …)` calls are just noisy.

---

## Scorecard — state at track creation (2026-07-22)

| Dimension | Grade | Evidence |
|---|---|---|
| Correctness & safety discipline | **A** | Security invariants are executable AST/TLS tests that self-test their own scanners; every printer error path wipes plaintext; the SSE subscribe-before-snapshot ordering is deliberate and documented |
| Comment / rationale quality | **A−** | Unusually high — comments explain *why*, name the alternative rejected, and cite the plan section. Docked for two stale ones (R3) |
| Structural seams & testability | **B+** | Good injected seams (`dispatch.Deps`, `Hub.DeleteBlob`, package-var `printDocument`/`fetchBlob`). Missing seams for Redis key names and test-server construction |
| Efficiency | **A−** | Nothing pathological. One real N+1 (R8), one known+documented full scan (`GetSenderByEmail`), one triple-parse in the hot dispatch path (R11) |
| **Duplication control** | **B−** | The weak axis, and the reason this track exists. 17 Redis-key string builds *(closed by R1)*, 4 token-hash implementations, 11 near-identical proxy routes, 3 test-server constructors, 5 copies of a shell teardown trap |
| Dead code / rot | **A−** | No dead code found. Two comments describe a world that no longer exists |
| **Overall** | **B+ (8/10)** | Careful, deliberate work by someone who understood the trade-offs. Repetitive rather than sloppy — the better failure mode, and the cheaper one to fix |

**The one-sentence version:** the thinking is A-grade and the typing is B-grade;
almost everything in this track is mechanical, and none of it requires rethinking
a design decision.

**Re-score after the track completes** and record the new grades in the Status
Log — the point of the scorecard is the delta, not the absolute letter.

---

## Refactoring Track Process Rules (apply to every R-goal)

1. **Zero behaviour change is the definition of done.** If a change alters a
   response body, a status code, a wire format, a persisted value, or a rendered
   string, it is out of scope. Stop and record it rather than proceeding — a
   "refactor" that changes behaviour is an unreviewed feature.
2. **If you have to edit a test assertion, you changed behaviour.** Existing
   tests must pass *unchanged* unless the goal explicitly says otherwise (only
   R9, which refactors the test scaffolding itself, is exempt — and it has its
   own falsification step).
3. **One goal per run, fresh context, one commit.** Clean subject + body, **no AI
   co-author trailer** (`CLAUDE.md`). Do not batch goals.
4. **These are not roadmap phases** — the `phase-implementer` workflow does not
   apply. The `plan-checker` agent runs at the end of each *session* (Goal RQ),
   not per goal: a refactor should leave the tree exactly as compliant with
   `plans/` as it was, so anything it reports is a real regression.
5. **The security invariants from GOALS.md §3 still bind.** They are not
   restated here because nothing in this track should go near them — if a goal
   seems to require touching `encrypted_key` handling, plaintext lifetime, or
   mTLS, that is a signal the goal has been misread.
6. **Start on a clean tree.** These are whole-file restructurings; an unrelated
   uncommitted change makes the diff unreviewable and the revert unsafe.
7. **Docker is only needed for R10.** Everything else verifies with the Go tests,
   Vitest, `tsc`, and `next build`. Where a goal touches e2e-observable markup,
   its Acceptance says how to verify without the full stack.
8. **Deleting a goal is a valid outcome.** If executing one reveals the
   duplication was load-bearing, or the abstraction costs more than it saves,
   record that in the Status Log and mark it `wontfix`. Three items are already
   marked that way (see *Considered and deliberately rejected*) — that judgement
   is part of the work, not a failure of it.

---

# Session A — "make drift impossible"

Small, fully covered by existing tests, and each one removes a place where a
copy-paste divergence becomes a silent security or routing bug. Highest value per
minute in this track by a wide margin. Est. ~1 h total.

## Goal R1 — Centralise Redis key and channel names

**Status:** done — 2026-07-22

**The problem.** The same four Redis names were built by string concatenation in
**8 non-test sites across 4 packages**, and **9 more times in tests**:

| String | Sites |
|---|---|
| `"mailbox:<id>:state"` | `store/redis.go:40`, `:82` |
| `"mailbox:<id>:dispatch"` | `link/hub.go:82` (sub), `dispatch/route.go:242` (pub) |
| `"mailbox:<id>:available"` | `link/hub.go:135` (pub), `dispatch/dispatcher.go:25` (psub pattern) |
| `"job:<id>:status"` | `link/hub.go:188` (pub), `handlers/jobs.go:284` (sub) |

Three of the four have their **publisher and subscriber in different packages**,
joined only by two string literals matching. A typo does not fail to compile,
does not fail a unit test, and does not log — the publish reaches zero
subscribers and the job sits in `queued` forever. This is precisely the failure
`dispatch/route.go:303` already writes a paragraph of operator diagnostics to
help debug. These strings are the system's real interface between packages, and
they are currently untyped.

**Do this.**

1. Add constructors to `services/cloud/store` (the package both sides already
   import):
   ```go
   func KeyPrinterState(mailboxID string) string
   func ChanDispatch(mailboxID string) string
   func ChanAvailable(mailboxID string) string
   func ChanJobStatus(jobID string) string
   const PatternAvailable = "mailbox:*:available"
   ```
   Give the group **one** doc comment explaining that these are the wire contract
   between packages and why they must not be inlined again — cite the
   publisher-and-subscriber-in-different-packages failure mode.
2. Replace all 8 non-test call sites.
3. Replace the test sites too — `services/cloud/{integration_redis,stream,
   hub_blob}_test.go`, `services/cloud/dispatch/dispatcher_test.go`,
   `services/cloud/link/hub_integration_test.go`. A test that hard-codes the
   string while the code uses the constructor tests nothing.
4. Leave `e2e/*.go` alone — separate module, asserts from outside the boundary.
5. Do not change any of the name formats.

**Acceptance:** `cd services/cloud && go build ./... && go vet ./... && go test ./...`
green with no test assertion edited; `grep -rn '"mailbox:"\|"job:"' --include=*.go
services/cloud/` returns only the new constructors. One commit.

---

## Goal R2 — One token hash, one token generator, one cookie builder

**Status:** done — 2026-07-22

> **CRITICAL — read before editing.** The hash output is **persisted**:
> `jobs.guest_token_hash` and `refresh_tokens.token_hash` hold values produced by
> this code. The algorithm and encoding (SHA-256 → `base64.RawURLEncoding`) must
> stay exactly as they are. This goal consolidates four copies of that
> computation into one; it must not change what the computation produces, or
> every stored token is invalidated.

**The problem — three related duplications in the auth handlers.**

*Token hashing is implemented four times*, all byte-identical:
`handlers/auth.go:31-32` (in `newRefreshToken`), `:132-133` (in `Refresh`),
`:169-170` (in `Logout`), and `handlers/jobs.go:79-82` (`hashGuestToken`, the only
named one). `jobs.go:75` explicitly says it is *"the single definition of how a
raw guest token maps to the stored value… so the two sides can never drift"* —
the right instinct, applied to guest tokens only. If the hash ever changes,
`Logout` silently stops matching what `Refresh` stored and logout stops revoking
sessions. No current test catches it, because each test exercises a single
consistent path.

*Token generation is written twice*: `newRefreshToken` (auth.go:25) and
`newGuestToken` (jobs.go:88) are the same function — 32 random bytes → base64 →
hash.

*The refresh cookie's five attributes are typed twice*: set in
`setRefreshCookie` (auth.go:36-46), re-typed to clear it in `Logout`
(auth.go:173-181). A browser only deletes a cookie when the clearing
`Set-Cookie` matches on **name, path, and domain**. If those drift, `Logout`
returns 204 and leaves a live session cookie — silent, security-relevant, and
invisible to a handler test that only checks the status code.

**Do this.**

1. One `hashToken(raw string) string` in the handlers package. Delete the three
   inline copies; re-point `hashGuestToken` at it (or delete it and call
   `hashToken` directly, keeping a comment at the guest-token call site noting
   that this value is stored, never the raw token).
2. One `newOpaqueToken() (raw, hash string, err error)`. Delete `newRefreshToken`
   and `newGuestToken`.
3. One `refreshCookie(value string, expires time.Time, maxAge int) *http.Cookie`
   used by both `setRefreshCookie` and `Logout`.

> **Do NOT change the cookie's `Path` (`/auth/refresh`) or name
> (`refresh_token`).** Both are a cross-language contract: the portal's
> `lib/proxy.ts` rewrites `Path=/auth/refresh` with a **regex**, and
> `middleware.ts` gates account pages on the cookie name. Changing either breaks
> every session's survival across a page reload, silently, with no error
> anywhere. That coupling is Goal R12's job — do not attempt it here.

**Acceptance:** `cd services/cloud && go build ./... && go vet ./... && go test ./...`
green; `phase8_test.go` (register/login/refresh) passes with **no assertion
edited**. One commit.

---

## Goal R3 — Retire two stale comments

**Status:** pending

Comment-only change, no code. Stale comments are worse than absent ones: they are
confidently wrong, and this codebase's comments are otherwise trustworthy enough
that a reader will believe them.

1. **`services/cloud/store/redis.go:53-59`** — `GetPrinterState`'s doc says *"No
   printer has ever connected before Phase 3 … Real dispatch logic (Phase 4) will
   need to treat 'idle with no slot entry for this slot' as 'unknown capacity'
   rather than always-available, but Phase 2 skips dispatch entirely so that
   distinction doesn't matter yet."* Phase 4 shipped, and `dispatch.eligible`
   (`dispatch/route.go:292-307`) **does** handle that case, with a specific
   diagnostic. Rewrite it to describe what the function does now and why the
   always-idle default is still right for the dispatch path — the **contrast**
   with `LookupPrinterState` (which the ops dashboard uses to tell offline from
   idle) is the thing worth documenting.
2. **`services/cloud/link/hub.go:131-134`** — *"No dispatcher subscribes yet in
   Phase 3 — PUBLISH with zero subscribers is a harmless no-op."* One does
   (`dispatch/dispatcher.go`). Keep the point that a zero-subscriber PUBLISH is
   harmless — it still is when no node runs a dispatcher — but stop asserting
   that nothing subscribes.

Do not restructure the code around them.

**Acceptance:** `cd services/cloud && go build ./... && go test ./...` green;
both comments describe current behaviour. One commit (may be bundled with R1 or
R2 if they land in the same session).

---

# Session B — "make it smaller"

Portal first (freshest context, no Docker), then Go, then shell last.
Est. ~4 h total.

## Goal R4 — Extract the SSE job-stream hook and status block

**Status:** pending

**The problem.** `services/portal/app/track/page.tsx` and
`services/portal/app/jobs/[id]/page.tsx` share ~60 lines:

- an identical `stamp()` time formatter, copy-pasted into both
- the same EventSource lifecycle — `onmessage` → parse → `setCurrent` →
  first-sighting `setTimes` → terminal check → close, plus `onerror` (~35 lines)
- the same `.status` block markup (live-dot + "Current status:" + `<strong>` +
  `<JobProgress>` + conditional `<DeliveredStamp>`)

They differ only in the stream URL's credential (`?token=` for guests vs
`?access=` for authenticated senders) and whether an auth guard runs first.

**Provenance, stated plainly:** this duplication was introduced by the portal
redesign. `journey.tsx` correctly extracted the *visual* and left the *stateful
stream logic* copied on both sides. Worth fixing while the context is fresh
rather than letting it set.

**Do this.**

1. Add `useJobStream(url: string | null)` (in `app/journey.tsx` or a new
   `app/use-job-stream.ts`) returning
   `{ current, times, statuses, error, connected, start, stop }`.
   Preserve exactly: **first-sighting timestamps** (a repeated status must not
   overwrite the recorded time — the stream is at-least-once), the
   terminal-status close, and the `onerror` behaviour where an intentional
   `stop()` must not surface "Connection lost".
2. Add a `<JobStatus current times />` component owning the `.status` block.
3. Rewrite both pages against them. `/track` keeps its form and Track/Reconnect
   button; `/jobs/[id]` keeps its auth guard and its "Opening live stream…"
   placeholder.

> **Markup constraints — the e2e suite asserts on these.**
> `.status strong` must contain the status word and **nothing else** (Playwright
> does `toHaveText("delivered")` against `textContent`, exact match — do not add
> a second `<strong>` inside `.status`); the literal text `"Current status:"` must
> remain visible in one element; the button name must still match
> `/Track|Reconnect/` on `/track`. See `services/portal/e2e/{guest,account}.spec.ts`.

**Acceptance:** `cd services/portal && npx tsc --noEmit -p tsconfig.json &&
npm test && npm run build` green. Then verify the e2e-observable markup without
Docker: `npx next start -p 3111`, drive `/track` and `/jobs/<id>` with a
throwaway Playwright script that fulfils `**/api/jobs/*/stream*` with
`content-type: text/event-stream` and a body of `data: {"status":"…"}\n\n` frames
for the full ladder, assert `.status strong` reaches `"delivered"` and
`getByText("Current status:")` is visible, then **delete the script** — it is not
part of the suite. One commit.

---

## Goal R5 — One guarded-fetch hook for account pages

**Status:** pending

**Depends on R4** (both touch `jobs/[id]/page.tsx`; doing R4 first avoids a
conflict).

**The problem.** `services/portal/lib/admin.ts` already encapsulates exactly this
sequence — wait for the auth bootstrap, redirect unauthenticated users to
`/login?next=<pathname>`, fetch, bounce on 401, set an error state, clean up with
an `active` flag. `app/history/page.tsx:22-56` writes all of it again, and
`app/jobs/[id]/page.tsx:29-33` repeats the guard preamble a third time.

**Do this.**

1. Rename `useAdminData` → `useGuardedData` and move it to
   `lib/guarded-data.ts`. It was never admin-specific: the `403`/`forbidden`
   branch simply never fires for non-admin endpoints. Update the three `/admin`
   pages' imports.
2. Rewrite `history/page.tsx` to use it:
   `useGuardedData<{ jobs: HistoryJob[] }>("/api/jobs")`. Keep the loading
   skeleton, the empty state, and the "N sent · M delivered" summary — only the
   fetch/guard plumbing goes.

> **Constraint.** `e2e/account.spec.ts` asserts `table.history tbody tr` has an
> exact row count and that `a[href="/jobs/<id>"]` is visible. The table markup
> must survive intact, including the `data-label` attributes that drive the
> mobile stacked-card layout.

**Acceptance:** `npx tsc --noEmit -p tsconfig.json && npm test && npm run build`
green; with the `refresh_token` cookie set and `**/api/auth/refresh` +
`**/api/jobs` stubbed, `/history` renders one row per job and the track link
resolves. One commit.

---

## Goal R6 — Collapse the portal API proxy routes

**Status:** pending

**The problem.** 11 of the 12 `app/api/**/route.ts` files are one of two
identical shapes — seven files (eight handlers) doing
`fetch(CLOUD_API_URL + path, { headers: forwardAuth(req) }) → proxyJSON()`, and
four auth files doing `req.text() → fetch → proxyWithCookies()`. Only
`jobs/[id]/stream/route.ts` is genuinely bespoke.

**Do this.**

1. Add a `relay()` helper to `lib/proxy.ts` covering both shapes (method,
   optional body pass-through, auth forwarding, which responder).
2. Reduce each boilerplate route body to one line.
3. **Keep every route's existing comment.** They document each route's auth mode
   and why — that is the part carrying the information, and it must survive even
   though the code it describes shrinks. Do not summarise or merge them.
4. Leave `jobs/[id]/stream/route.ts` completely alone.

> **Constraints.** `proxyJSON` must keep forwarding the `x-automail-node` header
> and must keep forwarding **nothing else** — `lib/proxy.test.ts` has a test
> asserting no other upstream header (including `set-cookie`) leaks through, and
> it must pass unchanged. `proxyWithCookies` must keep rewriting
> `Path=/auth/refresh` → `Path=/` (see R12). Every route keeps
> `export const dynamic = "force-dynamic"`.

> **Stop condition.** These files *are* the portal's security boundary, and there
> is value in a reviewer being able to `cat` them and see the whole answer to
> "what does the portal forward upstream?". If `relay()` ends up needing more
> than ~3 options to cover the cases, that is the signal this goal is not worth
> doing — mark it `wontfix`, record why, and move on.

**Acceptance:** `npx tsc --noEmit -p tsconfig.json && npm test && npm run build`
green with `proxy.test.ts` unedited. One commit.

---

## Goal R7 — Remove the AdminMailboxes N+1

**Status:** pending

**The problem.** `handlers/admin.go:156` calls `store.LookupPrinterState` inside
the `for _, mb := range mailboxes` loop — one Redis GET per mailbox. This is the
only genuine N+1 in the tree. At three mailboxes it is invisible; the dashboard
polls every 15s, so at 1,000 units it is 1,000 sequential GETs every 15 seconds
per open dashboard. It is also exactly what an interviewer probes for after you
have said "12M mailboxes".

**Do this.**

1. Add to `services/cloud/store`:
   `LookupPrinterStates(ctx, rdb, mailboxIDs []string) (map[string]PrinterState, error)`,
   using MGET or a pipeline. Keys absent from Redis must be **absent from the
   returned map** — that preserves the existing `found` semantics, which
   `AdminMailboxes` relies on to report "offline" vs "idle".
2. Rewrite `AdminMailboxes` to collect the ids, make one call, and read from the
   map. Behaviour must be identical: no live entry → status `"offline"` with DB
   slot capacity and occupancy 0; live entry with empty `Status` → `"idle"`.
3. Keep the singular `LookupPrinterState` — dispatch still uses it via
   `GetPrinterState` for one mailbox.

*Note:* `phase9_test.go` uses miniredis. Confirm the command chosen is supported
there (miniredis implements MGET; go-redis pipelines also work against it). If
one is unsupported, use the other rather than weakening the test.

**Acceptance:** `go build ./... && go vet ./... && go test ./...` green;
`TestAdminMailboxes_LiveAndOfflineStatus` passes with its assertions
**unchanged**. One commit.

---

## Goal R8 — One test-server constructor and a query router

**Status:** pending

Test-only change; **no non-test file may be touched**.

**The problem.**

- Three near-identical constructors, each doing `sql.OpenDB(fakeConnector{q})` →
  `t.Cleanup` → `&handlers.Server{…}`: `newAuthTestServer` (phase8_test.go:31),
  `newAdminTestServer` (phase9_test.go:34), `newStreamNode` (stream_test.go:98)
- `sql.OpenDB(fakeConnector{…})` at **9** sites
- **22** blocks of
  `switch { case strings.HasPrefix(query, "-- name: X"): return cols, rows, nil }`
  across phase8 / phase9 / stream / hub_blob tests

**Do this.**

1. In `dbfake_test.go` (or a new `testsupport_test.go`), add one
   `newTestServer(t *testing.T, q fakeQueryFunc, opts ...testServerOpt) *handlers.Server`
   with functional options `withRedis(rdb)`, `withJWT(priv)`. Re-point the three
   constructors at it — `newStreamNode` keeps its `httptest`/mux wiring on top,
   only the `Server` construction is shared.
2. Add a `routeQueries` helper replacing the prefix switches. **An unmatched
   query must fail loudly** (`t.Fatalf` or a distinctive error), never return
   zero rows — a silent miss turns a broken test into a passing one. Allow a
   result to carry a func instead of static rows so the existing dynamic cases
   (counters, closures over test state) keep working.
3. Do not change what any test asserts.

**Acceptance:** `go test ./... -count=1` green. Then the falsification step:
deliberately break one production behaviour (e.g. flip a status string in
`handlers/admin.go`), re-run, confirm the relevant test **fails**, and revert. A
test-scaffolding refactor that stops detecting regressions is worse than the
duplication it removed. One commit.

---

## Goal R9 — Shared shell stack harness

**Status:** pending — **needs Docker**, and gates CI. Go slowly.

**The problem.**

- `cleanup()` + `trap cleanup EXIT` is **byte-identical** in
  `scripts/e2e/run.sh`, `scripts/e2e/full.sh`, and `scripts/e2e/chaos.sh`, and
  near-identical in `scripts/load/run.sh` and `scripts/deploy/smoke.sh` — the
  latter two have **already drifted** (they add `--remove-orphans`). That drift
  is the tell.
- The `KEEP_STACK` debugging affordance is implemented five times.
- **11** hand-rolled `for i in $(seq 1 N); do … sleep 1; done` readiness loops
  across six scripts, each with its own timeout and failure message.

**Do this.**

1. Add `scripts/lib/stack.sh` providing `stack_init <compose files…>` (sets
   `COMPOSE`, arms the EXIT trap), `stack_up` (honours `NO_BUILD`), `stack_down`
   (honours `KEEP_STACK`, always `--remove-orphans`), and
   `wait_for <description> <timeout-seconds> <command…>`. `wait_for` must print
   the description, poll, and on timeout fail with the description **and the last
   command output** — the current loops' failure messages are the useful part and
   must not get worse.
2. Convert the scripts **one at a time**, running each to green before moving on:
   `e2e/run.sh` → `e2e/full.sh` → `e2e/chaos.sh` → `load/run.sh` →
   `deploy/smoke.sh`.
3. Preserve every script's env knobs (`KEEP_STACK`, `NO_BUILD`,
   `ALLOW_DESTRUCTIVE`) and, in `deploy/smoke.sh`, the fact that the trap is
   armed **only after preflight** — a preflight failure has nothing to tear down
   and must not claim otherwise.
4. Out of scope: `scripts/e2e/bootstrap.sh` (no stack) and `scripts/demo/up.sh`
   (its own richer flow).

> **If a script cannot be run to green locally, stop** after converting the ones
> you can verify and record which were left. Never convert a CI-gating script you
> cannot execute.

**Acceptance:** every converted script runs end to end; `.github/workflows/`
invocations still match. One commit (or one per script if they land separately —
record which in the log). One commit per run either way.

---

# Opportunistic

## Goal R10 — Tier 3 sweep

**Status:** pending — low priority; fold into another edit of the same file if
the chance comes up first.

Do **only** these, and skip any that turns out to be more invasive than one small
function:

| # | Item | Where |
|---|---|---|
| a | `rfc3339Ptr(sql.NullTime) *string` — replaces three copies of the `if .Valid { s := .Time.UTC().Format(time.RFC3339); x = &s }` pattern | `handlers/jobs.go:433`, `handlers/admin.go:100`, `:195` |
| b | `decodePEMFile(path) (*pem.Block, error)` shared by `loadRSAPrivateKey`/`loadRSAPublicKey`; also delete the stray blank line after `os.ReadFile` in both and **keep both `#nosec G304` comments** | `cloud/main.go:29`, `:48` |
| c | `JobRefFromValues` — replace the five identical `if …; err != nil` blocks with a loop over `[]struct{dst *string; key string}` | `dispatch/route.go:96` |
| d | Parse the job UUID **once** per dispatch instead of three times; `claimJob` and `revert` take a `uuid.UUID`. Keep the error behaviour identical for an unparseable id | `dispatch/route.go:257`, `:351`, `:387` |
| e | Package-level logger for `services/cloud/link` with the `"printer-link: "` prefix, replacing ~20 repetitions | `link/hub.go` |
| f | `decodeJSON[T](w, r) (T, bool)` generic helper for the 4 copies of the decode + `INVALID_BODY` block | `jobs.go:40`, `:109`, `auth.go:90`, `:200` |
| g | `historyJob`/`adminJob` share an embedded `jobDTO`; admin keeps its extra fields | `jobs.go:398`, `admin.go:29` |

**Acceptance:** `go build ./... && go vet ./... && go test ./...` green, no test
assertion edited. One commit.

---

## Goal R11 — Make the cross-language cookie contract loud

**Status:** pending

This one **adds a guard** rather than removing lines — the only goal in the track
that does.

**The problem.** `Path: "/auth/refresh"` is written twice in Go
(`handlers/auth.go:40`, `:176`) and matched by a **regex in TypeScript**
(`portal/lib/proxy.ts` `rewriteCookiePath`, `/Path=\/auth\/refresh/gi`). The
cookie *name* `refresh_token` is written twice in Go and read again in
`portal/middleware.ts:11`.

This is R1's problem across a language boundary, where it cannot be fixed with a
shared constant. If the cloud's path changes, the portal's regex silently stops
matching, the cookie keeps `Path=/auth/refresh`, the browser stops sending it to
`/api/auth/refresh`, and **every session silently fails to survive a page
reload** — with no error anywhere. The portal middleware's gate also stops seeing
the cookie, so account pages start bouncing to `/login`.

**Do this.** There is no clean deduplication available; make the coupling loud
instead of silent.

1. Add a comment on the **Go** side naming this as a contract and pointing at
   `lib/proxy.ts`. The portal side already half-documents it; the Go side says
   nothing.
2. Make `rewriteCookiePath` detect a miss: if an incoming `Set-Cookie` named
   `refresh_token` has neither `Path=/auth/refresh` nor `Path=/`, log a warning
   server-side (not in the response). A silent regex miss is the entire failure
   mode.
3. Add a `lib/proxy.test.ts` case asserting the mismatch is surfaced, not
   swallowed.

**Do not change the path or the cookie name.**

**Acceptance:** `cd services/portal && npm test && npx tsc --noEmit -p tsconfig.json`
and `cd services/cloud && go build ./... && go test ./...` green. One commit.

---

## Goal RQ — Recurring refactor gate

**Status:** recurring (run at the end of each session — never marked done)

After Session A and again after Session B:

1. Run the `plan-checker` agent over the working tree. Every goal in this track
   is a pure refactor, so **anything it reports as drift from `plans/` is a real
   regression** introduced by the refactor. Report findings; do not fix silently.
2. `make ci` green (both Go modules, portal `tsc`/build/tests, coverage floors
   held — a refactor must not drop coverage; if it does, the deleted code was
   carrying tests that the replacement does not).
3. Re-run the counted greps from *How this was measured* for the items the
   session claimed to fix, and record the new numbers in the Status Log. "I
   removed the duplication" is a claim; the grep is the evidence.
4. Report the net line-count change per service against the ~450–500 line
   estimate, and re-score the affected scorecard rows.

---

## Considered and deliberately rejected

Recorded so a future pass does not re-litigate them. These are **`wontfix`**, not
`pending`.

| Item | Why not |
|---|---|
| **The printer's hand-rolled PBKDF2** (`printer/crypto.go:248`, 33 lines) | "Delete hand-rolled crypto" is right in general and wrong here, and the existing comment's justification checks out: stdlib `crypto/pbkdf2.Key` takes the password as a `string`, which is immutable and cannot be zeroed — converting the passphrase `[]byte` copies it into memory left to the GC. `golang.org/x/crypto/pbkdf2` does take `[]byte`, but the printer module currently depends on exactly **one** package (`nhooyr.io/websocket`), and adding a dependency to the most security-sensitive binary to save 33 lines of a well-specified, unit-tested KDF is a bad trade. The surrounding PBES2 ASN.1 walk is likewise unavoidable — stdlib parses only *unencrypted* PKCS#8 |
| **`internalError(w, msg)` for the 31 `WriteError(500, …)` calls** | The 31 distinct messages *are* the information. A helper would flatten them into one indistinguishable failure |
| **`zeroBytes` → the `clear()` builtin** | `defer clear(b)` is legal and does zero the slice (verified). But `zeroBytes` is a **named, documented security primitive** that the invariant tests and `docs/study/` reference. `clear` in a defer obscures intent in exactly the code where intent matters most |
| **`mustEnv` duplicated across modules** (`cloud/main.go:121`, `printer/config.go:38`) | Separate Go modules by design. 7 duplicated lines is cheaper than a shared module and its versioning relationship |
| **Sharing mTLS setup between cloud and printer** | Same reason, larger: the printer must be independently deployable onto field hardware (`plans/13`). A shared module is an architecture decision, not a refactor |
| **Collapsing the 12 portal route files into fewer files** | Next.js's App Router requires file-per-route. Only the bodies can shrink (R6) |
| **Unifying `hub.pumpDispatch`'s and `StreamJob`'s pub/sub select-loops** | Structurally similar, semantically different — one lives for a socket's lifetime and writes to a WebSocket, the other for an HTTP request and writes SSE frames with terminal shutdown. A shared generic would need enough callbacks to be longer than both copies |
| **`GetSenderByEmail`'s decrypt-compare full scan** | A real efficiency cost, but already known and documented at `db/queries.sql:64` with the right fix named (a deterministic blind-index column). That is a schema change with a migration — a feature, not a refactor. Belongs in `plans/13` if it is ever wanted |

---

## Status Log

| Date | Goal | Commit | Outcome |
|------|------|--------|---------|
| 2026-07-22 | Goal R2 | _(this commit)_ | **DONE.** New `handlers/tokens.go` holds the single definition: `hashToken` (SHA-256 → base64 RawURL) and `newOpaqueToken` (32 CSPRNG bytes + digest). The four hash copies are gone (`auth.go`'s `newRefreshToken`, `Refresh`, `Logout`, and `jobs.go`'s `hashGuestToken`); `newRefreshToken`/`newGuestToken` collapsed into one generator; the refresh cookie's five attributes now exist once in `refreshCookie(value, expires, maxAge)`, with `setRefreshCookie`/`clearRefreshCookie` as the two call sites and `refreshCookieName`/`refreshCookiePath` as named consts carrying a comment about the cross-language contract (R11). Acceptance greps: `sha256.Sum256` appears exactly **once** in `handlers/`, and the cookie attributes exactly once. **Method — the literals were pinned BEFORE the refactor.** A throwaway probe captured the current renderings, and all four matched values derived independently outside Go (`sha256sum \| xxd -r -p \| base64 \| tr '+/' '-_'`): digest of a fixed vector, digest of `""`, and the exact `Set-Cookie` strings for set and clear. `tokens_test.go` pins those, so "no behaviour change" is a passing test rather than a claim. **Both new guards were falsified, and both exposed a real pre-existing gap.** (1) Salting `hashToken` — which would invalidate every stored guest and refresh token — makes the new pinned-digest test fail, while the pre-existing edge tables (`hardening_test.go`, `jobs_test.go`) **all still passed**: they only check length 43, URL-safety, determinism and the create/verify round trip, every one of which survives a salt. (2) Changing `refreshCookiePath` to `/auth/refresh2` — which silently breaks the portal's regex and kills session persistence across a reload — makes the new rendering test fail, while `phase8_test.go`'s cookie check (name + HttpOnly only) **still passed**. Neither failure mode had a guard before this goal. A third subtest pins that set and clear agree on name and path, since a mismatch means the browser never deletes the cookie. **Deviation from the goal body, recorded per Process Rule 8:** the goal offered "re-point `hashGuestToken` at it *or* delete it"; I deleted it and renamed to `hashToken`/`newOpaqueToken`, because once refresh tokens share the function, "Guest" in the name is a new inaccuracy — the exact rot R3 exists to remove. That required editing two existing test files, so per Process Rule 2 the diff was reviewed line by line: **every** removed line is a comment, a test-function name, or a call to the renamed identifier. No expected value, input, or comparison target changed. Also fixed two comments left dangling by the rename (`stream_test.go:42`, `jobs.go:333`). Green: `gofmt`, `go vet` (default and `-tags integration`), `go test ./... -count=1`, `-race`, coverage **20.5 → 21.0** (floor ratcheted). |
| 2026-07-22 | Goal R1 | _(this commit)_ | **DONE.** New `store/keys.go` defines the four Redis names once (`KeyPrinterState`, `ChanDispatch`, `ChanAvailable`, `ChanJobStatus`, `PatternAvailable`) with one doc comment naming the publisher/subscriber-in-different-packages failure mode. All **8 non-test** sites converted (`store/redis.go` ×2, `link/hub.go` ×3, `dispatch/route.go`, `dispatch/dispatcher.go` — its private `availablePattern` const deleted — `handlers/jobs.go`) plus all **9 test** sites (`stream_test` ×2, `hub_blob_test`, `integration_redis_test` ×3, `link/hub_integration_test` ×2, `dispatch/dispatcher_test`). Acceptance grep is clean: the only remaining literals are the constructors themselves and their format test. **Count correction:** the track was created claiming "22 sites (8 non-test + 14 test)" — the real figure in `services/cloud` is **17 (8 + 9)**. The 14 came from a looser grep that also matched a `uniqueName("mailbox:disp")` throwaway fixture and an `e2e/` comment. Scorecard row corrected; the claim "every count is a real number" now holds. **Added `store/keys_test.go`, and it is not optional.** Falsifying the refactor showed why: with a deliberately broken `ChanDispatch` format, `link`'s tests still **passed** — because publisher and subscriber now share the constructor, they agree with each other even when the format is wrong. The refactor trades "the two sides can silently drift apart" (the original danger) for "both sides move together, so a format change is invisible to consumer tests". `keys_test.go` pins the literals as an independent copy and is the replacement guard; it fails loudly on the same break, and a second test pins that `PatternAvailable` actually matches what `ChanAvailable` produces (a pattern matching nothing is not an error). `store` went from *no test files* to covered; cloud coverage **20.2 → 20.5**, floor ratcheted per `scripts/coverage.floors`' own convention. **Left deliberately:** `e2e/chaos_test.go:215` still builds the state key inline — `e2e/` is a separate Go module asserting from outside the boundary and cannot import `automail/cloud/store`; coupling it would defeat the point of a black-box test. Green: `gofmt` clean, `go vet` (default **and** `-tags integration`), `go test ./... -count=1`, `go test ./... -race`, coverage gate. **No existing test assertion changed value** — `integration_redis_test.go:213`'s channel assertion changed *expression* (literal → constructor) but not the string it compares, which is the goal's intent. **Not committed:** the working tree carries four unrelated bodies of work (portal redesign, node header, plans/13, this track), so staging is left to the owner — `git add services/cloud/store/keys.go services/cloud/store/keys_test.go services/cloud/store/redis.go services/cloud/link/hub.go services/cloud/dispatch/ services/cloud/handlers/jobs.go services/cloud/stream_test.go services/cloud/hub_blob_test.go services/cloud/integration_redis_test.go scripts/coverage.floors` gives a clean isolated R1 commit. |
| 2026-07-22 | — | _(uncommitted)_ | **Track created.** Full-tree audit (~11.5k hand-written lines) → scorecard **B+ (8/10)** and 11 goals. Headline counts at creation: 22 Redis-key string builds across 4 packages (3 of 4 names have publisher and subscriber in *different* packages, coupled only by matching literals); 4 implementations of the persisted token hash; 2 copies of the refresh-cookie attributes (a drift here makes logout silently not log out); 11 of 12 portal API routes on one of two boilerplate shapes; 3 cloud test-server constructors + 22 `-- name:` prefix switches; a byte-identical shell teardown trap in 3 scripts, already drifted in 2 more; one genuine N+1 (`AdminMailboxes`). Also found while writing the goals: `Path=/auth/refresh` is a **cross-language** contract — a Go string literal matched by a TypeScript regex — whose silent failure mode is "no session survives a page reload, with no error anywhere" (now R11). Eight items assessed and marked `wontfix` with reasons, including the printer's hand-rolled PBKDF2 (justification verified against the stdlib signature — the `string` password cannot be zeroed) and `zeroBytes` → `clear()` (legal, verified, still rejected). Estimated reduction if the track completes: ~450–500 lines, zero behaviour change. All goals `pending`. |
