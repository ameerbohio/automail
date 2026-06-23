# Horizontal Scaling

## Stateless Server Contract

The cloud server holds **no authoritative in-process state**. Every request can be handled by any node, and the only in-process state — long-lived connections — is *recoverable soft state*: if the node holding it dies, the peer reconnects to another node and re-registers. This is enforced by design:

| Data type | Where it lives | In-process |
|---|---|---|
| Job records | PostgreSQL | never |
| Mailbox state cache | Redis (key: `mailbox:<id>:state`) | never |
| Job queue | Redis Streams | never |
| JWT validation key | RS256 public key (static, loaded at startup) | static |
| Session state | None — JWT is stateless | never |
| SSE subscriber connection | Held by one node; routed via Redis Pub/Sub | soft (recoverable) |
| Printer WebSocket connection | Held by one node; routed via Redis Pub/Sub | soft (recoverable) |

Any node can receive any request, and any node can dispatch to any printer, because all cross-node coordination goes through Redis. A held connection (SSE downstream, or a printer's persistent socket upstream) lives on exactly one node, but no authoritative state depends on *which* node — the connection is rebuilt on reconnect and job state stays in Postgres/Redis. The two long-lived connections are symmetric: SSE is a **fan-out** (one status event must reach the node holding a sender's connection), the printer link is a **fan-in** (a dispatch from any node must reach the node holding the printer's connection). Both are solved with the same Redis pub/sub bridge.

---

## Job Queue: Redis Streams + Consumer Groups

Redis Streams provide exactly-once delivery across multiple consumers. This is the correct tool when N nodes compete to dispatch jobs and exactly one should win.

### Publishing a Job

When a job cannot be dispatched immediately (printer busy or slot full):

```go
// XADD: append to stream, Redis assigns an ID
jobID, _ := redis.XAdd(ctx, &redis.XAddArgs{
    Stream: "jobs:pending",
    Values: map[string]any{
        "job_id":        job.ID,
        "mailbox_id":    job.MailboxID,
        "encrypted_key": base64(job.EncryptedKey),
        "blob_ref":      job.BlobRef,
    },
}).Result()
```

### Consuming Jobs (dispatcher goroutine, one per node)

```go
// Each node is a consumer in the same group
redis.XGroupCreateMkStream(ctx, "jobs:pending", "dispatchers", "$")

for {
    msgs, _ := redis.XReadGroup(ctx, &redis.XReadGroupArgs{
        Group:    "dispatchers",
        Consumer: nodeID,        // unique per node instance
        Streams:  []string{"jobs:pending", ">"},
        Count:    1,
        Block:    5 * time.Second,
    }).Result()

    for _, msg := range msgs[0].Messages {
        if dispatch(ctx, msg) {
            redis.XAck(ctx, "jobs:pending", "dispatchers", msg.ID)
        }
        // If dispatch fails: do not ACK; message stays pending
    }
}
```

### Crash Recovery (XAUTOCLAIM)

If a node crashes mid-dispatch, its unacknowledged messages sit in the Pending Entry List (PEL). Another node reclaims them:

```go
// Run periodically by each node
msgs, _, _ := redis.XAutoClaim(ctx, &redis.XAutoClaimArgs{
    Stream:   "jobs:pending",
    Group:    "dispatchers",
    Consumer: nodeID,
    MinIdle:  60 * time.Second,  // reclaim messages idle > 60s
    Start:    "0-0",
    Count:    10,
}).Result()
// Re-attempt dispatch for each reclaimed message
```

---

## Dispatch Eligibility Check

Before dispatching a job, the cloud server checks current printer state from Redis:

```go
type PrinterState struct {
    Status       string            // "idle" | "printing"
    SlotOccupancy map[string]SlotInfo  // slot_id → {current, max}
    UpdatedAt    time.Time
}

func canDispatch(ctx context.Context, jobMailboxID, jobSlotID string) bool {
    var state PrinterState
    redis.Get(ctx, "mailbox:"+jobMailboxID+":state").Scan(&state)
    if state.Status != "idle" { return false }
    slot := state.SlotOccupancy[jobSlotID]
    return slot.Current < slot.Max
}
```

To prevent two nodes from dispatching the same job simultaneously, the cloud server uses `SELECT FOR UPDATE` in Postgres before routing the dispatch to the mailbox:

```sql
-- Atomic job claim
BEGIN;
SELECT id FROM jobs WHERE id = $1 AND status = 'queued' FOR UPDATE NOWAIT;
UPDATE jobs SET status = 'dispatching' WHERE id = $1;
COMMIT;
```

If the `FOR UPDATE NOWAIT` fails (another node holds the lock), the current node skips this job and moves on. After a successful claim, the node does not call the printer directly — it publishes the dispatch to `mailbox:<id>:dispatch` (see below), since the printer's socket may be held by a different node.

---

## SSE Fan-Out via Redis Pub/Sub

A sender's SSE connection is long-lived and may land on any node. The printer's status frame arrives on whichever node owns its WebSocket (Node A), but the SSE connection may be held by a different node (Node B); the update must cross from A to B.

```
Printer → status frame over WebSocket → Cloud Node A (owns printer socket)
  Node A → PUBLISH redis "job:<id>:status" { status: "delivered" }
  Node B (holds SSE connection) ← SUBSCRIBE "job:<id>:status"
  Node B → write SSE event to sender's browser
```

In Go (Node B — the node holding the SSE connection):

```go
pubsub := redis.Subscribe(ctx, "job:"+jobID+":status")
ch := pubsub.Channel()
for msg := range ch {
    fmt.Fprintf(w, "data: %s\n\n", msg.Payload)
    flusher.Flush()
}
```

No sticky sessions required. Any node can hold any SSE connection.

---

## Printer Connection Fan-In via Redis Pub/Sub

The printer dials out and holds **one** persistent mTLS WebSocket, terminated on a single node (the *owner node*). But the dispatcher goroutine runs on every node, so the node that claims a job is often **not** the node holding that printer's socket. A node cannot write to a socket it does not hold — this is the inverse of the SSE problem, and it is solved the same way.

```
Owner node A accepts printer socket → SUBSCRIBE redis "mailbox:<id>:dispatch"
Node B claims a job (SELECT FOR UPDATE NOWAIT) → PUBLISH "mailbox:<id>:dispatch" { job_id, encrypted_key, blob_url }
Node A receives the message → writes a dispatch frame down the WebSocket
Printer decrypts, prints, wipes → sends status frame back up the same socket
Node A handles the status frame → PUBLISH "job:<id>:status" → SSE fan-out to the sender
```

In Go (Node A — the node that owns the printer socket):

```go
sub := redis.Subscribe(ctx, "mailbox:"+mailboxID+":dispatch")
for msg := range sub.Channel() {
    conn.WriteMessage(websocket.TextMessage, dispatchFrame(msg.Payload))
}
```

**Presence and liveness.** `PUBLISH` returns the number of subscribers. A return of `0` means no node holds a live socket for that printer — it is offline — so the dispatching node re-queues the job instead of losing it. The owner node also refreshes `mailbox:<id>:state` (TTL ~90s) from `state` frames and WebSocket pings; when the socket closes, that key expires and the printer is considered offline. A dropped connection is therefore a cleaner offline signal than a missed heartbeat POST: the transport itself reports it.

**Failover.** If the owner node crashes, its socket drops; the printer's reconnect-with-backoff loop dials again and lands on a surviving node (Docker round-robins the initial dial), which re-registers it and re-subscribes. No dispatch is lost — unacknowledged jobs remain in the Redis Stream PEL and are reclaimed via `XAUTOCLAIM`.

---

## Health Checks

```go
// GET /healthz — called by Traefik every 10s
func healthz(w http.ResponseWriter, r *http.Request) {
    // Check Postgres connectivity
    if err := db.PingContext(r.Context()); err != nil {
        http.Error(w, "db unreachable", 503)
        return
    }
    // Check Redis connectivity
    if err := rdb.Ping(r.Context()).Err(); err != nil {
        http.Error(w, "redis unreachable", 503)
        return
    }
    w.WriteHeader(200)
}
```

Traefik removes unhealthy nodes from the rotation automatically.

---

## Running Multiple Nodes (Prototype)

```bash
# Start 2 cloud server instances on the same Proxmox VM
docker compose up --scale cloud-server=2

# Traefik round-robins between them automatically
# Redis Streams consumer group ensures no duplicate dispatch
```

Each instance gets a unique `nodeID` (e.g., `$HOSTNAME` or a UUID generated at startup) used as the Redis consumer name.

---

## Production Scaling Path (noted, not built)

| Current (prototype) | Production path |
|---|---|
| Docker Compose `--scale` | Kubernetes Deployment, HPA |
| Self-hosted Postgres | AWS RDS / Cloud SQL with read replicas |
| Self-hosted Redis | AWS ElastiCache / Redis Cloud with clustering |
| Self-signed certs | Cert-Manager + Let's Encrypt |
| Home server | Cloud VPC with private subnets |
