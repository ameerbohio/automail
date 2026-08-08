# Graceful shutdown & consumer lifecycle

*Code: `services/cloud/main.go`, `services/cloud/shutdown.go`, `services/cloud/dispatch/dispatcher.go`, `services/cloud/link/hub.go`, `services/cloud/handlers/{health,jobs}.go`. Plan: `plans/16-kubernetes.md` §4. Acceptance: `make shutdown-check`.*

## What it is

Everything the cloud server does between receiving SIGTERM and exiting: stop taking new work, finish what it holds, tell its peers it is leaving, and remove its own name from the shared Redis consumer group. Before this, `main.go` ended in `ListenAndServe()` + `log.Fatal` with no signal handling at all — the process died the instant the kernel delivered SIGTERM.

## Why it suddenly mattered

Nothing about the code changed; the *scheduler* did.

Under Docker Compose a restart is a rare, human-initiated event, so an abrupt death is invisible. Under Kubernetes, SIGTERM is routine: every rolling update, every eviction, every HPA scale-down sends it — **to a pod that is often still receiving traffic**, because removing the pod from Endpoints and signalling it are two independent, racing operations.

So the same code that looked fine for a year has three visible symptoms on a cluster:

| What dies | Symptom |
|---|---|
| In-flight `POST /jobs` | Client sees a connection reset mid-submission |
| Open SSE stream | Status page goes dead with no terminal event |
| Printer WebSocket | Printer waits out a TCP timeout before re-dialling |

Plus one that accumulates silently: the Redis consumer group grows by one dead entry per pod, forever.

## The four things that make it more than three lines

`signal.NotifyContext` → `server.Shutdown(ctx)` is the textbook answer, and it is insufficient here for four specific reasons.

### 1. `Shutdown` does not cancel in-flight request contexts

It stops accepting, closes idle connections, and then **waits** for active handlers to return. It never cancels `r.Context()` — that fires when the *connection* drops.

`StreamJob` blocks in a `select` on `r.Context().Done()`. So every open SSE stream would pin `Shutdown` for the entire grace period and then be severed by process exit anyway: the worst of both. The fix is an explicit second signal — a `drain` channel closed at the top of the sequence, which the handler also selects on:

```go
case <-s.Drain:
    writeSSEComment(w, flusher, "draining: server shutting down, reconnect")
    return
```

That comment is the whole point of the design being *observable*. A killed process and a drained one both close the socket instantly; only a drained one writes something on the way out. The acceptance test asserts on the wire content, not on timing, for exactly that reason.

The client needs no change: `EventSource` reconnects automatically, lands on a pod that is not draining, and re-syncs from the handler's initial DB snapshot.

### 2. There were two `http.Server`s and only one was reachable

`startMTLSServer` created its server inside the function, blocked on it, and exposed no handle. Draining the public listener while the internal one dies with the process leaves the printer link exactly as broken as having no signal handling. It is now `newMTLSServer`, returning the `*http.Server` so shutdown can address it.

### 3. The printer link is a *hijacked* connection

`Shutdown` does not wait for hijacked connections and does not close them — the WebSocket just dies with the process. `Hub.Close` walks the registry and sends `websocket.StatusGoingAway`, which is the difference between the printer re-dialling in milliseconds and waiting out a TCP timeout.

### 4. Cancelling the dispatcher mid-dispatch is not free

`drain`/`reclaim`/`handle` all took the Run loop's context, so cancelling it aborts an attempt already underway. That is the one moment when a message can end up **neither ACKed nor dispatched** — precisely when the next step deletes the consumer that owns it. So `handle` runs on `context.WithoutCancel(callerCtx)` under its own `handleTimeout`: cancellation stops the loop *reading*, never stops the attempt *in hand*.

## Consumer-group cleanup, and the trap in it

Measured on this project before the work started: **4 consumers registered in `dispatchers` for 3 live nodes** — one left by a container that no longer existed. Stale consumers are not only untidy; a dead one holding a pending message keeps that message in its PEL until `XAUTOCLAIM`'s `MinIdle` elapses, which is added dispatch latency on every rollout.

The obvious fix — `XGROUP DELCONSUMER` on shutdown — has a trap that is easy to state and expensive to discover:

> **`XGROUP DELCONSUMER` discards the consumer's pending-entries list.** The entries are dropped from the group, not handed back for `XAUTOCLAIM` to reclaim.

So "ACK what you can and leave the rest for reclaim, then delete the consumer" is self-contradictory: the delete destroys exactly what you left. Both the shutdown path and the reaper therefore check `XPENDING` for that consumer first and **skip the delete when the PEL is non-empty**. A consumer left behind is harmless; a consumer deleted with work in it is a lost job.

DELCONSUMER-on-shutdown only helps when shutdown happens, so a periodic reaper covers OOMKill and node loss. Its idle threshold is stated *relative to* the reclaim window — `reapMinIdle = 5 × claimMinIdle` — so a consumer is never reaped inside the window where `XAUTOCLAIM` is still the expected recovery path. A live node touches Redis on every drain (at least once per sweep interval), so it is never close to the threshold.

## Readiness vs liveness

`/healthz` checks Postgres and Redis. That is a correct **readiness** probe and a terrible **liveness** probe: a Redis blip would make Kubernetes restart every cloud pod simultaneously, converting a dependency wobble into a total outage.

| | `/healthz` (readiness) | `/livez` (liveness) |
|---|---|---|
| Question | Should traffic be routed here? | Is this process wedged? |
| Checks dependencies | yes | **no** |
| While draining | **503** — stop routing here | **200** — it is alive and finishing |

The draining row is the one people get wrong in both directions. Readiness must fail so the endpoints controller stops sending new requests; liveness must *not*, or the kubelet SIGKILLs a process that is shutting down correctly.

The portal got a matching `app/api/healthz` route for the same reason: Kubernetes marks a pod Ready the moment the container starts, which for Next.js is before it is listening — so `maxUnavailable: 0` without a readiness probe still drops requests.

## The ordering, and why it is that order

```
signal-drain      → release long-lived handlers; readiness starts failing
dispatcher-loop   → stop reading new work; wait for the loop to return
consumer-group    → only now is the PEL final, so DELCONSUMER is safe
printer-sockets   → going-away, so printers re-dial elsewhere immediately
public-listener   → Shutdown: finish in-flight requests
internal-listener → Shutdown
```

Two deliberate policies: all steps share **one** deadline (a wedged step burns the budget and the rest return immediately, rather than each waiting out its own timeout past the SIGKILL), and a failed step **does not abort** the sequence — the later steps matter most when something has already gone wrong.

## The honest caveat (the follow-up an interviewer asks)

**"How do you know the drain actually worked, rather than the process just dying fast?"**

You can't tell from timing — an unhandled SIGTERM also closes every socket instantly. Three independent signals are asserted instead:

1. **Exit code 0.** An unhandled SIGTERM exits 143; a SIGKILL after the grace period exits 137. Only a process that handled the signal and returned from `main` exits 0.
2. **The `: draining` SSE comment**, which a killed process cannot write.
3. **The consumer count**, cycled `up → stop → up` at `--scale 3`: 3 → 0 → 3. Before this work it only ever grew.

**"What is still not proven?"** The grace period is a budget, not a guarantee: if a step wedges, the container's `stop_grace_period` (30s, set above the process's 20s `SHUTDOWN_TIMEOUT` on purpose) or Kubernetes' `terminationGracePeriodSeconds` SIGKILLs it mid-drain, and the consumer is then left for the reaper. That is the designed fallback, not an accident — but it means the guarantee is "clean shutdown when the dependencies respond", not "clean shutdown always".

**"Why not just let `XAUTOCLAIM` handle everything and skip DELCONSUMER?"** It would be correct but slower: every rollout would leave its messages parked for `MinIdle` before anyone reclaimed them, and the group would grow without bound. The cleanup is a latency and hygiene optimisation on top of a recovery mechanism that already works.
