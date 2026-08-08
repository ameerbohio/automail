package dispatch

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"automail/cloud/store"

	"github.com/redis/go-redis/v9"
)

// claimMinIdle matches plans/03-scaling.md's XAUTOCLAIM example: a pending
// entry idle longer than this (its consumer never XACK'd it, most likely
// because that node crashed mid-dispatch) is fair game for another node
// to reclaim and retry.
const claimMinIdle = 60 * time.Second

// handleTimeout bounds a single dispatch attempt. handle deliberately runs on
// a context *detached* from Run's (see its doc comment), so this is the only
// thing that stops a wedged Postgres/Redis call from holding shutdown open --
// it must stay comfortably below main.go's shutdown grace.
const handleTimeout = 15 * time.Second

// reapMinIdle is how long a consumer must have gone without touching Redis
// before the reaper deletes it. It is deliberately a multiple of
// claimMinIdle: a dead consumer's pending entries only become reclaimable
// after claimMinIdle, and reclaiming reassigns them to the *live* consumer
// that claimed them (emptying the dead one's PEL). Reaping at 5x leaves ample
// room for a sweep to have run first, so the reaper never races the reclaim
// path. A live node refreshes its idle clock on every XREADGROUP, and Run
// drains at least once per sweepInterval (= claimMinIdle), so a healthy node
// is never within an order of magnitude of this threshold.
const reapMinIdle = 5 * claimMinIdle

// The dispatcher subscribes once to every mailbox's availability channel via
// PSUBSCRIBE (store.PatternAvailable) rather than one SUBSCRIBE per
// mailbox_id: it has no registry of which mailboxes exist (that's exactly the
// kind of authoritative state plans/03-scaling.md says nodes must not hold),
// so a pattern subscription is what lets it react to any printer going idle
// without first discovering its ID.

// Dispatcher is the one-per-node goroutine that drains jobs:pending as
// printers become available, per plans/05-cloud-server.md "Dispatcher
// Goroutine". Re-attempts share attemptDispatch's core logic with the
// immediate-dispatch path in route.go, via Retry.
type Dispatcher struct {
	Deps     Deps
	NodeID   string // unique per node instance; the Redis consumer name
	SweepInt time.Duration
	// ReapIdle overrides reapMinIdle (tests only -- waiting out the real
	// threshold would make the suite minutes long). Zero means the default.
	ReapIdle time.Duration
}

// EnsureGroup creates the jobs:pending consumer group if it doesn't exist
// yet. Safe to call on every node startup -- BUSYGROUP ("group already
// exists") is expected and ignored after the first node creates it.
func (di *Dispatcher) EnsureGroup(ctx context.Context) error {
	err := di.Deps.Redis.XGroupCreateMkStream(ctx, PendingStream, ConsumerGroup, "$").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// Run blocks until ctx is cancelled, draining jobs:pending whenever a
// mailbox:<id>:available event fires and periodically sweeping for
// crashed-node leftovers via XAUTOCLAIM and for stale consumers left behind
// by nodes that died ungracefully. Intended to be started once per node in a
// goroutine from main.go.
//
// Cancelling ctx stops this node *reading* new work and returns promptly;
// any dispatch already in progress finishes on a detached context (see
// handle), so a message is never left neither ACKed nor dispatched at the
// moment main.go removes this consumer from the group.
func (di *Dispatcher) Run(ctx context.Context) {
	sub := di.Deps.Redis.PSubscribe(ctx, store.PatternAvailable)
	defer sub.Close()
	ch := sub.Channel()

	sweep := time.NewTicker(di.sweepInterval())
	defer sweep.Stop()

	// Drain once on startup too -- jobs may already be sitting in the
	// stream from before this node existed (or restarted).
	di.drain(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			di.drain(ctx)
		case <-sweep.C:
			di.reclaim(ctx)
			di.drain(ctx)
			di.ReapStaleConsumers(ctx)
		}
	}
}

func (di *Dispatcher) sweepInterval() time.Duration {
	if di.SweepInt > 0 {
		return di.SweepInt
	}
	return claimMinIdle
}

// maxDrainBatches caps how many XREADGROUP round trips a single drain()
// call makes against *new* (">") messages, so a burst larger than that
// doesn't hold the goroutine hostage before it returns to Run's select
// loop -- still-blocked messages are left pending (not re-XADD'd, see
// Retry), so this cap is about fairness/latency, not runaway growth.
const maxDrainBatches = 20

// drain reads available messages from jobs:pending for this node's
// consumer in bounded batches and attempts a Retry dispatch for each --
// see handle's doc comment for which outcomes get XACK'd.
func (di *Dispatcher) drain(ctx context.Context) {
	for i := 0; i < maxDrainBatches; i++ {
		streams, err := di.Deps.Redis.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    ConsumerGroup,
			Consumer: di.NodeID,
			Streams:  []string{PendingStream, ">"},
			Count:    10,
			// Negative Block omits BLOCK entirely (non-blocking poll) --
			// Run's select loop decides when to call drain. Block: 0 would
			// send BLOCK 0 ("wait forever"), pinning this goroutine inside
			// XREADGROUP so the available-event and sweep cases in Run never
			// fire again.
			Block: -1,
		}).Result()
		if err != nil {
			if !errors.Is(err, redis.Nil) {
				log.Printf("dispatch: XREADGROUP: %v", err)
			}
			return
		}
		if len(streams) == 0 || len(streams[0].Messages) == 0 {
			return
		}
		for _, msg := range streams[0].Messages {
			di.handle(ctx, msg)
			// Shutdown landed mid-batch: finish the message in hand (handle
			// already ran it to completion on a detached context) and stop
			// reading. The rest of the batch stays in this consumer's PEL,
			// which is exactly what RemoveConsumer checks before deleting it.
			if ctx.Err() != nil {
				return
			}
		}
	}
}

// handle re-attempts dispatch for one stream message via Retry (not
// TryDispatch -- this message is already in jobs:pending, so a still-
// blocked outcome must not XADD a duplicate, see Retry's doc comment).
// The message is XACK'd whenever Retry reports done=true -- the job
// either actually dispatched, or its row already settled into a
// terminal/claimed state that makes this stream entry stale (nothing
// left to retry either way) -- or when the message itself is malformed
// beyond retrying. A still-blocked attempt is left un-ACK'd in the PEL so
// the next mailbox:<id>:available event or XAUTOCLAIM sweep retries the
// same entry rather than multiplying it.
//
// The attempt runs on a context detached from the caller's (bounded by
// handleTimeout instead). Cancelling Run's context on SIGTERM must stop this
// node reading *new* messages, but aborting an attempt already underway is
// the one thing that could leave a message neither dispatched nor ACK'd at
// precisely the moment shutdown deletes this consumer from the group
// (plans/16-kubernetes.md §4.1). Finishing the attempt is bounded and cheap;
// getting it wrong loses a job.
func (di *Dispatcher) handle(callerCtx context.Context, msg redis.XMessage) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(callerCtx), handleTimeout)
	defer cancel()

	fields, err := JobRefFromValues(msg.Values)
	if err != nil {
		log.Printf("dispatch: malformed stream message %s: %v", msg.ID, err)
		// Can't be retried into anything valid -- ack it off the PEL so it
		// doesn't sit there forever.
		di.ack(ctx, msg.ID)
		return
	}

	done, err := Retry(ctx, di.Deps, fields)
	if err != nil {
		log.Printf("dispatch: job %s: retry failed: %v", fields.JobID, err)
		return // leave pending; next sweep retries
	}
	if !done {
		return // still blocked; leave pending, don't multiply the entry
	}
	di.ack(ctx, msg.ID)
}

func (di *Dispatcher) ack(ctx context.Context, id string) {
	if err := di.Deps.Redis.XAck(ctx, PendingStream, ConsumerGroup, id).Err(); err != nil {
		log.Printf("dispatch: XACK %s: %v", id, err)
	}
}

// reclaim runs XAUTOCLAIM to recover jobs left in the Pending Entries
// List by a node that crashed mid-dispatch (plans/03-scaling.md "Crash
// Recovery"). Claimed messages are handed to the same handle() path as
// drain's normal flow; XReadGroup is not involved here since the
// messages are already in this consumer's PEL once XAUTOCLAIM reassigns
// them.
func (di *Dispatcher) reclaim(ctx context.Context) {
	start := "0-0"
	for {
		msgs, next, err := di.Deps.Redis.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream:   PendingStream,
			Group:    ConsumerGroup,
			Consumer: di.NodeID,
			MinIdle:  claimMinIdle,
			Start:    start,
			Count:    10,
		}).Result()
		if err != nil {
			log.Printf("dispatch: XAUTOCLAIM: %v", err)
			return
		}
		for _, msg := range msgs {
			log.Printf("dispatch: reclaimed job from crashed consumer: %s", msg.ID)
			di.handle(ctx, msg)
			if ctx.Err() != nil {
				return // shutting down; see drain's matching guard
			}
		}
		if next == "0-0" || len(msgs) == 0 {
			return
		}
		start = next
	}
}

// RemoveConsumer deletes this node's consumer from the jobs:pending group.
// Called once on graceful shutdown, after Run has returned, so the group
// reflects the set of *live* nodes instead of growing by one entry per
// container that ever existed (plans/16-kubernetes.md §4.2 — 4 consumers were
// measured for 3 live nodes under Compose; a k8s Deployment leaks one per pod
// per rollout).
//
// It is deliberately a no-op when this consumer still holds pending entries:
// XGROUP DELCONSUMER *discards* the consumer's PEL rather than handing those
// entries back, so deleting a consumer that still owns un-ACK'd messages
// destroys the jobs XAUTOCLAIM was going to reclaim. Leaving the consumer is
// harmless -- the reaper will take it once reclaim has drained its PEL.
func (di *Dispatcher) RemoveConsumer(ctx context.Context) error {
	deleted, err := di.deleteConsumerIfDrained(ctx, di.NodeID)
	if err != nil {
		return err
	}
	if !deleted {
		log.Printf("dispatch: consumer %s still has pending entries; left in the group for XAUTOCLAIM", di.NodeID)
		return nil
	}
	log.Printf("dispatch: removed consumer %s from group %s", di.NodeID, ConsumerGroup)
	return nil
}

// deleteConsumerIfDrained removes name from the consumer group unless it
// still owns PEL entries. Reports whether the delete actually happened.
func (di *Dispatcher) deleteConsumerIfDrained(ctx context.Context, name string) (bool, error) {
	pending, err := di.Deps.Redis.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream:   PendingStream,
		Group:    ConsumerGroup,
		Start:    "-",
		End:      "+",
		Count:    1,
		Consumer: name,
	}).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, err
	}
	if len(pending) > 0 {
		return false, nil
	}
	if err := di.Deps.Redis.XGroupDelConsumer(ctx, PendingStream, ConsumerGroup, name).Err(); err != nil {
		return false, err
	}
	return true, nil
}

func (di *Dispatcher) reapIdle() time.Duration {
	if di.ReapIdle > 0 {
		return di.ReapIdle
	}
	return reapMinIdle
}

// ReapStaleConsumers deletes consumers that have not touched Redis for
// reapMinIdle. RemoveConsumer only helps when a node shuts down gracefully;
// an OOMKill, a lost node or a `kill -9` leaves its consumer behind forever,
// so this is the belt to that braces (plans/16-kubernetes.md §4.2).
//
// Same PEL rule as RemoveConsumer, for the same reason: a consumer that still
// owns un-ACK'd entries is skipped, not deleted, so reaping can never destroy
// work that XAUTOCLAIM is about to reclaim. This node's own consumer is
// skipped outright -- it is by definition live.
func (di *Dispatcher) ReapStaleConsumers(ctx context.Context) {
	consumers, err := di.Deps.Redis.XInfoConsumers(ctx, PendingStream, ConsumerGroup).Result()
	if err != nil {
		// NOGROUP before the first job is ordinary, not an error worth noise.
		if !strings.Contains(err.Error(), "NOGROUP") && !errors.Is(err, redis.Nil) {
			log.Printf("dispatch: XINFO CONSUMERS: %v", err)
		}
		return
	}
	for _, c := range consumers {
		if c.Name == di.NodeID || c.Idle < di.reapIdle() || c.Pending > 0 {
			continue
		}
		deleted, err := di.deleteConsumerIfDrained(ctx, c.Name)
		if err != nil {
			log.Printf("dispatch: reap consumer %s: %v", c.Name, err)
			continue
		}
		if deleted {
			log.Printf("dispatch: reaped stale consumer %s (idle %s)", c.Name, c.Idle)
		}
	}
}
