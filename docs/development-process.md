# Development Process (how this codebase actually gets built)

This is a process document, not a concept note (see `docs/study/` for those). It records the actual development loop used while building Automail with Claude as a drafting partner — worth studying alongside the code, because the *how* generalizes to other AI-assisted projects even when the specific bugs don't.

## 1. The per-phase cycle

The roadmap (`plans/10-implementation-roadmap.md`) breaks the build into ~10 phases, each with a fixed task list and a one-line **Verify** acceptance test. Every phase follows the same four-step loop, defined in `CLAUDE.md`:

1. **Implement** exactly one phase — no more. A dedicated `phase-implementer` subagent does this, scoped tightly to that phase's task list. It is explicitly told *not* to start the next phase's work even if it would be convenient (e.g. "while I'm here, let me also wire up the SSE relay").
2. **Verify** with a second, independent subagent (`plan-checker`) that re-reads the plans and the phase's Verify line cold, with no memory of what the implementer believed it built. It checks the working tree against the spec, not against the implementer's own summary.
3. **Fix drift** — anything plan-checker flags gets corrected before moving on.
4. **Commit** — one phase per commit, clean message, no AI co-author trailer (project convention).

**Why two separate subagents instead of one agent self-checking its own work**: an agent that just wrote code is primed to believe it's correct — it reasons from "what I intended to build" rather than "what the spec actually requires." A fresh subagent with no memory of the implementation session has no such anchor; it can only compare the diff against the written spec. This is the same reason code review works better as a second pair of eyes than as a self-review.

## 2. The audit-driven bugfix loop

Separately from new-phase work, completed phases get periodically re-audited against the plans (not just the roadmap's Verify lines, but the full security/architecture docs). The loop:

1. Spawn a read-only audit (again, a fresh agent/pass with no attachment to the existing code) that produces a **concrete punch list**: file:line, one-sentence problem, grouped by severity (security invariant violations first, then correctness bugs, then spec drift, then nits).
2. Work the list **one item at a time**: for each item —
   - **Confirm it's real before fixing it.** Several items on the punch list turned out to be deliberate, already-tested design decisions (see §4) rather than bugs. Fixing first and asking later would have silently undone a decision someone already made and verified.
   - Apply the smallest fix that addresses the root cause.
   - Re-run the build/vet/test suite immediately, not at the end of the batch — a regression introduced by fix #3 should be caught before fix #4 starts, not after all eight are in.
3. Commit the whole batch as one "review findings" commit, separate from any new phase work, so the diff a reviewer reads is self-contained and the phase commits stay clean.

**Why not just fix everything found and move on**: a punch list from a fresh-eyes audit will sometimes flag something that's actually fine — intentional scope deferral, an already-tested tradeoff, two plan documents disagreeing with each other where the code correctly picked one. Treat every finding as a *hypothesis to verify*, not an instruction to execute blindly.

## 3. Triage: fix now vs. defer vs. not a bug

Three outcomes are equally valid for any audit finding, and conflating them is the most common way an AI-assisted change set balloons past what was asked:

- **Fix now** — a real defect with no design tension (e.g. a host port binding that breaks a documented Verify step; an unused generated query; a Go version mismatch between two services).
- **Defer, documented** — real but out of scope for the current phase (e.g. `mailboxes.status`/`last_heartbeat_at` were flagged as "never written to Postgres," but the roadmap's Phase 3 task list and Verify line only ever specified the Redis cache — Postgres mirroring is Phase 9's job). The right move is to confirm against the roadmap that it's genuinely future work, not to implement it preemptively.
- **Not a bug** — the audit's hypothesis doesn't survive contact with the code. Example below.

## 4. Concrete example: when "fix the finding" was wrong

An audit flagged that the printer-link status payload omitted `job_id`, citing the documented SSE wire format in `plans/09-api-contracts.md`. The instinct was to add `job_id` back into `link.statusPayload` in `services/cloud/link/frames.go`.

Running the test suite *before* committing that change surfaced `frames_test.go`'s `TestJSONStatusPayload_OmitsErrorWhenEmpty`, which asserts the **opposite** — that `job_id` must *not* be present, with an explicit comment: "caller already knows it from the channel name." That assertion encodes a deliberate, already-reviewed design decision (the Redis pub/sub channel name already scopes the job, so repeating the ID in the payload is redundant), not an oversight.

The actual fix was narrower: the two plan documents disagreed with each other (`05-cloud-server.md`'s pseudocode omitted `job_id`; `09-api-contracts.md`'s SSE wire example included it). Both were corrected to agree — the *internal* Redis payload stays narrow, and a note was added explaining that the future SSE handler (Phase 5) is responsible for re-adding `job_id` when it relays the payload to a client, since the handler already knows the job ID from the URL path. No production code changed; only the spec's internal contradiction did.

**Lesson**: a failing pre-existing test after your "fix" is a signal to re-examine the fix, not to update or delete the test. Tests that assert a counter-intuitive negative ("must NOT contain X") are usually guarding exactly this kind of well-intentioned regression.

## 5. Debugging a flaky test: read the dependency's source, don't guess

`TestHub_RegisterSeedsStateAndDispatchReachesSocket` failed intermittently (~30-40% of runs) on "expected the hub to be subscribed to the mailbox's dispatch channel." The methodology that found the real cause:

1. **Reproduce reliably first.** A single run proves nothing about a race; looping the test 5-25 times with `-count=1` (which disables Go's test result cache) turns "sometimes fails" into a measurable failure rate.
2. **Read the actual library source, not assumptions about it.** The hypothesis was a generic "Redis pub/sub is eventually consistent" hand-wave. Reading `go-redis`'s `pubsub.go` and `redis.go` directly (via `go env GOMODCACHE` → the vendored source on disk) showed precisely that `Client.Subscribe()` writes the SUBSCRIBE command over the wire and returns *without waiting for the server's acknowledgement*. That's a specific, fixable fact — not a vague "network is async" excuse.
3. **First fix attempt was incomplete — re-test before declaring victory.** Adding a `sub.Receive(ctx)` call to wait for the ack reduced but didn't eliminate the failures. Looping again immediately (rather than assuming the fix worked because it was "obviously correct") caught this.
4. **Trace the actual causal chain the test relies on.** The test's only synchronization signal was "the printer state key is visible in Redis." But in `hub.go`, that state key was being set *before* the subscribe call — so "state visible" never implied "subscription active" in the first place, no matter how the subscribe call itself was synchronized. The fix was to reorder: subscribe-and-wait-for-ack *first*, then seed the state key. Now the test's existing polling loop (which already waited for the state key) transitively guarantees the subscription is live, because both writes happen on the same goroutine in program order, and Redis (effectively single-threaded for command processing) processes them in that same order.
5. **Recognize the production implication.** This wasn't just a test artifact: in production, a job dispatched immediately after a printer reconnects could have hit the identical window and been wrongly requeued. The test flakiness was a real race surfacing under load, not a test-only problem — worth calling out explicitly rather than filing it as "fixed a flaky test."
6. **Verify with volume, including `-race`.** 25 consecutive passes plus a `go test -race` run before committing — a one-off green run after a concurrency fix proves much less than a fix to a description-level race should.

## 6. Commit discipline

- Cleanup/audit-fix work and new-phase work are **separate commits**, even within the same session, so each commit's diff answers one question ("what did phase N add" vs. "what did review catch").
- A fix found *while* implementing a phase (like the flaky test, discovered during phase 4 review) still gets its **own commit** once it's understood to be a distinct, separately-reviewable issue — not folded into the phase commit it happened to be found near.
- No commit happens before `go build && go vet && go test` (and, for anything touching goroutines/channels, `go test -race`) all pass locally.

## 7. Where to push back on automation

The project's own collaboration model (`plans/11-ai-collaboration.md`) sets the default to *teach first, draft after, do not silently expand scope*. In practice that showed up as:
- Stopping at each phase boundary for human review rather than auto-chaining phase 4 → phase 5 → phase 6.
- Asking before assuming an audit finding (like the Postgres mirroring gap) should be fixed immediately versus deferred to the phase that actually specifies it.
- Treating "the existing test disagrees with my fix" as a stop sign, not a prompt to change the test.
