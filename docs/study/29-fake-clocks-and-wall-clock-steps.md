# The CI Job That Failed With No Output: Wall Clocks, Monotonic Clocks, and a Lying Fake

A red `Coverage floor` job whose entire log was one line — `── cloud ──` — followed
by `Error: Process completed with exit code 1`. No test name, no assertion, no
panic. This note is the narrated version of that bug: why the gate went silent,
what actually failed underneath it, and why the culprit was the *fake* Redis
disagreeing with the real one about what time it is.

Code: [`scripts/coverage.sh`](../../scripts/coverage.sh),
[`services/cloud/dispatch/fakeredis_test.go`](../../services/cloud/dispatch/fakeredis_test.go),
[`services/cloud/dispatch/dispatcher_test.go`](../../services/cloud/dispatch/dispatcher_test.go).
The system under test is the dispatcher's consumer group — see
[14-redis-streams-consumer-groups.md](14-redis-streams-consumer-groups.md) and
[24-graceful-shutdown-consumer-lifecycle.md](24-graceful-shutdown-consumer-lifecycle.md).

There are two separate bugs here. They compound: one made the other invisible.

## Bug 1: a gate that fails without saying why

```bash
set -euo pipefail
out=$( cd "$ROOT/services/$m" && go test $pkgs -covermode=atomic -coverprofile="$prof" 2>&1 )
echo "$out" | grep -E 'coverage:|FAIL|panic' | sed 's/^/  /' || true
```

The intent was tidy output: capture the run, print only the interesting lines.
The effect under `set -e` is that **a failing `go test` kills the script on the
assignment line** — before the `echo` that would have shown what failed. The
entire failure report sat in `$out` and was discarded with the shell.

Redirecting `2>&1` into the capture made it worse: even the test binary's stderr,
which would otherwise have reached the terminal, was swallowed.

The fix keeps the tidy summary for the passing case and dumps everything on
failure, then carries on to the next module rather than aborting:

```bash
status=0
out=$( ... go test ... 2>&1 ) || status=$?
if [ "$status" -ne 0 ]; then
	echo "$out" | sed 's/^/  /'          # everything, not the filtered summary
	echo "  ✗ $m: go test failed (exit $status) — coverage not measured"
	fail=1
	continue                              # still gate the other module
fi
```

The rule worth stating in an interview: **`x=$(cmd)` under `set -e` is a trap.**
The assignment inherits the command's exit status, so the script dies holding the
only copy of the diagnostics. Capture the status explicitly whenever the output
matters more than the exit code.

## Bug 2: what was actually failing

Reproduced with sample count, the same technique as
[23-test-goroutine-lifetime-race.md](23-test-goroutine-lifetime-race.md):

```bash
GOMAXPROCS=1 go test ./dispatch/ -covermode=atomic -coverprofile=/dev/null -count=300 -cpu=1
--- FAIL: TestDispatcher_BlockedRetryStaysPendingWithoutDuplicating (0.40s)
    dispatcher_test.go:99: got 0 pending entries, want 1 (still-blocked job left un-ACK'd)
```

The test asserts the property the whole retry design rests on: a job that is still
blocked stays in the consumer's Pending Entries List, un-ACK'd, so the next
`mailbox:<id>:available` event or `XAUTOCLAIM` sweep retries *that* entry instead
of a duplicate.

`XPENDING` said the PEL was empty. Printing more state at the failure said
otherwise:

```
groups=[{Name:dispatchers Consumers:1 Pending:1 ...}] consumers=[{Name:test-node Pending:1 ...}]
```

`XINFO GROUPS` and `XINFO CONSUMERS` both report one pending entry. `XPENDING`
reports none. Two commands, one server, contradicting each other.

Four failures in that run, at **14:22:59, 14:23:29, 14:24:00, 14:24:30** — every
thirty seconds, almost to the second. Periodicity like that is never a scheduler
race; something on a timer is doing this.

## The mechanism: `time.Now().UTC()` throws away the monotonic clock

A Go `time.Time` from `time.Now()` carries **two** readings: a wall-clock reading
(what the calendar says, subject to NTP) and a monotonic reading (nanoseconds
since an arbitrary boot-relative origin, which never goes backwards). `Sub`,
`Since` and comparisons use the monotonic reading when both operands have one —
which is exactly why measuring an interval with `time.Now()` is safe even if NTP
steps the clock mid-measurement.

`UTC()`, `Round()`, `Truncate()`, `AddDate()` and marshalling all **strip** the
monotonic reading, because a wall-clock-only concept comes out the other side.
Subtraction then falls back to comparing wall clocks — and wall clocks move
backwards.

miniredis stamps and compares times with `time.Now().UTC()`
(`effectiveNow()`), and its `XPENDING` filters every entry through:

```go
timeSinceLastDelivery := now.Sub(p.lastDelivery)
if timeSinceLastDelivery >= idle {   // idle == 0 when the command carries no IDLE argument
    res = append(res, ...)
}
```

If the wall clock has moved *backwards* since `XREADGROUP` recorded
`lastDelivery`, that duration is negative, `>= 0` is false, and the entry vanishes
from the reply while remaining very much in the PEL.

And this machine's wall clock does move backwards. A ten-line probe settled it:

```
BACKWARD jump at 18:32:37.919: -1.784366243s
BACKWARD jump at 18:33:08.398: -1.773016219s
```

WSL2 resyncs `CLOCK_REALTIME` with the Windows host roughly every 30 seconds and
here lands ~1.8s behind each time. Any test iteration straddling a step sees an
empty PEL. One failure per step, four steps in the run — the arithmetic matches
the failure timestamps exactly.

## Why this is the fake's bug, not Redis's

Real Redis applies **no idle filter at all** unless the command carries an explicit
`IDLE` argument, and go-redis only sends one when `XPendingExtArgs.Idle` is set.
Against real Redis the entry is always returned. miniredis reimplements the filter
unconditionally with `idle` defaulting to zero, so a negative duration — a state
real Redis's code path can't even express here — silently drops rows.

That distinction is the whole point. The property under test is true; the double
was lying about it. Fixing the *assertion* to tolerate the lie would have been the
wrong move.

It also isn't only an assertion problem. Production
[`deleteConsumerIfDrained`](../../services/cloud/dispatch/dispatcher.go) probes the
PEL with the same shape of `XPENDING` before deleting a consumer:

```go
pending, err := di.Deps.Redis.XPendingExt(ctx, &redis.XPendingExtArgs{ ... Count: 1, Consumer: name })
if len(pending) > 0 { return false, nil }          // still owes entries — do not delete
```

Against the fake, mid-step, that reads as "drained" and proceeds to
`XGROUP DELCONSUMER` — which **discards** the consumer's PEL rather than handing
it back for `XAUTOCLAIM`. That is precisely the job-destroying case
`consumer_lifecycle_test.go` exists to forbid, and the fake could have staged it.
Correct against real Redis, tripped only by the double.

## The fix: pin the fake's clock

Every miniredis in the `dispatch` package now comes from one helper:

```go
func newFakeRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	mr.SetTime(time.Now())        // frozen: delivery-time arithmetic is exactly 0, never negative
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return mr, rdb
}
```

A frozen clock makes `now.Sub(lastDelivery)` exactly zero instead of occasionally
negative, which satisfies `>= 0` deterministically, on any host, whatever NTP is
doing. It is also the honest description of what these tests want: none of them
has any business caring what time it is.

The same run that produced four failures in 300 iterations was clean for 400
iterations (~125s, spanning four backward steps) afterwards, plus a clean
`-count=60` of the whole package.

The second change in that test was to stop synchronising on a sleep:

```go
// was: time.Sleep(100 * time.Millisecond) then assert receivers != 0
for {
	receivers, err := rdb.Publish(ctx, store.ChanAvailable(mailboxID), "1").Result()
	...
	if receivers > 0 { break }            // Run's PSUBSCRIBE has attached
	if time.Now().After(deadline) { t.Fatal("expected the dispatcher to be PSUBSCRIBEd ...") }
	time.Sleep(10 * time.Millisecond)
}
```

The assertion is unchanged — the dispatcher really must be subscribed — but the
guess about how long a loaded CI runner needs to schedule a goroutine is gone.
The *second* sleep in that test stays: it waits to prove nothing happened (no
duplicate `XADD`), and "assert an absence" genuinely needs a settle window.

## The honest caveat

Freezing the fake's clock means the fake can no longer exercise anything
*driven* by elapsed time — `XAUTOCLAIM`'s `MIN-IDLE-TIME` and the stale-consumer
reaper. That costs nothing here, because `claimMinIdle` is 60s and `reapMinIdle`
is 5 minutes: no unit test could ever have waited them out. Those paths are pinned
against real Redis in the `-tags=integration` suite (`make test-integration`),
which is where they belong — the fake never modelled consumer idle time anyway
(it reports `idle: -1`, a placeholder). A future test that does need time to pass
must move the fake forward explicitly with `mr.FastForward`.

The second caveat is about the CI run that started this. Because of Bug 1, its
output is gone, so this is the *most likely* cause and not a proven one: the
mechanism reproduces on demand locally and the failing job is the one that runs
these tests, but a backward wall-clock step on a GitHub runner is rarer than on
WSL2 (Azure hosts usually slew rather than step). The real remedy for that
uncertainty is Bug 1's fix — the next failure, whatever it is, will say so.

## Interviewer follow-ups

- *"Why didn't `go test ./... -race` catch it?"* It did not fail in that job on
  that run — this is a ~1-in-75 flake locally and rarer in CI, and the race
  detector has nothing to say about it: no memory is raced, a fake just returns a
  wrong answer.
- *"Why is `time.Now().UTC()` the problem when `time.Now()` alone isn't?"*
  `time.Now()` carries a monotonic reading that `Sub` prefers; `UTC()` strips it,
  so the subtraction degrades to wall-clock arithmetic that NTP can move
  backwards. The general rule: keep monotonic readings on any `time.Time` you
  intend to subtract, and never persist or compare *durations* derived from
  wall-clock-only values.
- *"Isn't freezing the clock hiding a real bug?"* No — the divergence is the
  fake's. Real Redis applies no idle filter without an `IDLE` argument. The
  time-driven behaviour the freeze does suppress is tested against real Redis
  instead.
- *"How would you have found it if it only ever failed in CI?"* Fix the harness
  first. A gate that fails without output is unfalsifiable; every hypothesis after
  that is guesswork.
