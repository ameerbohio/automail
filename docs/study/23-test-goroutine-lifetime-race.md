# The `-race` Failure That Only Happened in CI: `testing.T` and Goroutine Lifetime

A green local suite, a red CI job, and a `WARNING: DATA RACE` whose stack contains
no Automail code at all — only `testing` internals. This note is the narrated
version of that bug: what raced, why CI found it and a laptop didn't, and the rule
that prevents the whole class.

Code: [`services/cloud/stream_test.go`](../../services/cloud/stream_test.go)
(`newPrinterLinkNode`), [`services/cloud/hub_blob_test.go`](../../services/cloud/hub_blob_test.go),
[`services/cloud/link/hub_integration_test.go`](../../services/cloud/link/hub_integration_test.go).
The system under test is the printer-link hub — see
[11-dispatch-fan-in-printer-link.md](11-dispatch-fan-in-printer-link.md).

## What it is

The failing report, trimmed:

```
WARNING: DATA RACE
Read at 0x00c0003dc04b by goroutine 63:
  testing.(*common).destination()
  testing.(*common).log()
  testing.(*common).Logf()
  automail/cloud.TestHubDeletesBlobOnDelivered.func2()
      services/cloud/hub_blob_test.go:93
  net/http.HandlerFunc.ServeHTTP()
  net/http.(*conn).serve()

Previous write at 0x00c0003dc04b by goroutine 56:
  testing.tRunner.func1()          <- the test function's own teardown
```

Two goroutines touch the same `*testing.T`: the **httptest handler goroutine**
calls `t.Logf`, while the **test runner goroutine** is in `tRunner`'s deferred
epilogue marking that same `T` finished. The contested address is a field inside
`testing.common` (the "am I done?" state `log`/`destination` consult). Neither is
Automail state — the race is over the test harness itself.

The offending line was innocent-looking diagnostics:

```go
node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    if err := hub.Accept(r.Context(), w, r); err != nil {
        t.Logf("hub.Accept returned: %v", err)   // <- runs after the test returned
    }
}))
defer node.Close()
```

## Why it happens: `Close()` does not wait for hijacked connections

`httptest.Server.Close` is documented to block until outstanding requests finish,
and normally it does — it tracks every connection through the `ConnState` hook and
waits on a `WaitGroup`. But `hub.Accept` performs a WebSocket upgrade, which
**hijacks** the connection, and `net/http/httptest/server.go` says:

```go
case http.StateHijacked, http.StateClosed:
    // Remove c from the set of tracked conns and decrement it from the
    // waitgroup, unless it was previously removed.
```

Once hijacked, the server stops tracking that connection. It has to: after a
hijack, `net/http` no longer owns the socket, so it has no idea when the handler
is done. The consequence is that `node.Close()` returns immediately for our
WebSocket handler, and the handler goroutine outlives the test function.

The sequence that produced the race:

1. Test body finishes its assertions and returns.
2. Deferred `printer.CloseNow()` closes the printer side of the socket.
3. Deferred `node.Close()` returns without waiting (hijacked conn, untracked).
4. `tRunner`'s deferred epilogue starts tearing the `*testing.T` down — **write**.
5. Meanwhile the handler's `readLoop` finally observes the EOF from step 2,
   `Accept` returns the error, and the handler calls `t.Logf` — **read**.

Steps 4 and 5 are unordered. That is the whole bug.

`t.Log` and friends *are* safe for concurrent use — but only for the **lifetime of
the test**. `testing` even has an explicit guard for the losing interleaving
(`panic: Log in goroutine after test has completed`). We hit the race detector
rather than that panic because the detector flags the unsynchronized access first,
before the state it protects is even read consistently.

## Why CI caught it and the laptop didn't

The window between "handler wakes up" and "test teardown runs" is microseconds
wide, and locally the handler almost always wins. CI widened it: shared runners
with fewer, busier cores, and `-race` itself adding roughly 5–10× slowdown and
extra scheduling points. Same code, different odds.

That is also how it was reproduced locally — not by staring at it, but by raising
the number of samples:

```bash
go test ./ -race -count=60 -run 'TestHubDeletesBlobOnDelivered|...'
```

Three runs, three failures pre-fix; 180 iterations clean afterward. **A flake you
can reproduce on demand is just a bug.** Getting to `-count=N` (or `-cpu`, or a
loaded machine) before attempting a fix is what separates "fixed it" from "it
stopped happening."

## The fix

Never call `*testing.T` from a goroutine whose lifetime the test does not control.
Hand the value back to the test goroutine instead — the handler stashes the error
in a buffered channel, and `t.Cleanup` (which runs while the `T` is still alive)
reports it:

```go
func newPrinterLinkNode(t *testing.T, hub *link.Hub, label string) *httptest.Server {
	t.Helper()
	acceptErr := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := hub.Accept(r.Context(), w, r); err != nil {
			select {
			case acceptErr <- err:
			default: // only the first error is interesting
			}
		}
	}))
	t.Cleanup(func() {
		srv.Close()
		select {
		case err := <-acceptErr:
			t.Logf("%s hub.Accept returned: %v", label, err)
		default:
		}
	})
	return srv
}
```

Details that matter:

- **Buffered, size 1, with a `default`.** A send must never block a goroutine
  nobody is joining, and after the test ends nobody is receiving.
- **`t.Cleanup`, not `defer`.** Cleanups run after the test body's own defers
  (so after `printer.CloseNow()` gives the handler its reason to exit) but still
  inside the test's lifetime, which is exactly the window where `t.Logf` is legal.
- The diagnostic survives. Deleting the `t.Logf` would also have fixed the race,
  but the error text (`read frame header: EOF`) is what you want when the test
  fails for real.

Three call sites converged on this helper — the two blob-lifecycle tests and the
cross-node fan-out test. `link/hub_integration_test.go` already carried the
pattern and a comment explaining it; the fix propagated it to the sites that were
written later and didn't inherit it.

## Alternatives considered

- **Join the handler goroutine** (`done := make(chan struct{})`, `close(done)` on
  exit, wait in cleanup). Strictly stronger — it would make the goroutine's
  completion deterministic — but it deadlocks the moment a test `t.Fatal`s before
  dialing (nothing ever runs the handler, so nothing closes `done`), and it
  couples every test to the hub's shutdown latency. Not worth it for a log line.
- **`t.Errorf` instead of `t.Logf`.** Same race, plus it can fail the *next* test
  in some interleavings. `t.Fatal` from a non-test goroutine is worse still: it
  calls `runtime.Goexit` on the wrong goroutine, so the test never actually stops.
- **Drop the log.** Loses the diagnostic; see above.

The repo already states this rule for the fake DB driver in
[`dbfake_test.go`](../../services/cloud/dbfake_test.go): *"fakes must not call
`testing.T` from server goroutines."* Same principle, different server.

## The honest caveat

This does not stop the handler goroutine from outliving the test — it only makes
that harmless. The goroutine still runs `Accept`'s deferred `Registry.Remove` and
`sub.Close()` after the test has moved on. For these tests that is fine: each one
gets a fresh hub, a fresh miniredis, and a fresh fake DB, so a straggler has
nothing shared to corrupt. If a test ever needed to *assert* on post-teardown
effects (say, that the registry is empty after the socket drops), a channel-based
join would become mandatory — and a package-wide `goleak.VerifyTestMain` would be
the natural next step, since nothing currently proves these goroutines exit at all.

Worth stating plainly at the end of an interview answer: **this was a test bug,
not a product bug.** `hub.Accept` was never racy; the test's own observability
was. Being able to make that distinction — and still fix it rather than retry the
job — is the point.

## Interviewer follow-ups

- *"Why did the race detector point at `testing` and not your code?"* Because the
  shared state was the `*testing.T`, mutated by `tRunner`'s epilogue and read by
  `t.Logf`. The detector reports the memory access, not the design mistake.
- *"Why doesn't `Server.Close()` wait?"* After a hijack, `net/http` has given the
  connection away and can't know when the handler finishes; httptest deletes the
  conn from its tracking set at `StateHijacked`.
- *"How do you know it's fixed and not just quieter?"* It reproduced at
  `-count=60` before the change and survived 180 iterations after, plus the full
  `make test-race` gate across both modules.
