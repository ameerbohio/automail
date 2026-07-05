package dispatch

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// claimMinIdle matches plans/03-scaling.md's XAUTOCLAIM example: a pending
// entry idle longer than this (its consumer never XACK'd it, most likely
// because that node crashed mid-dispatch) is fair game for another node
// to reclaim and retry.
const claimMinIdle = 60 * time.Second

// availablePattern subscribes once to every mailbox's availability
// channel via PSUBSCRIBE rather than one SUBSCRIBE per mailbox_id: the
// dispatcher has no registry of which mailboxes exist (that's exactly the
// kind of authoritative state plans/03-scaling.md says nodes must not
// hold), so a pattern subscription is what lets it react to any printer
// going idle without first discovering its ID.
const availablePattern = "mailbox:*:available"

// Dispatcher is the one-per-node goroutine that drains jobs:pending as
// printers become available, per plans/05-cloud-server.md "Dispatcher
// Goroutine". Re-attempts share attemptDispatch's core logic with the
// immediate-dispatch path in route.go, via Retry.
type Dispatcher struct {
	Deps     Deps
	NodeID   string // unique per node instance; the Redis consumer name
	SweepInt time.Duration
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
// crashed-node leftovers via XAUTOCLAIM. Intended to be started once per
// node in a goroutine from main.go.
func (di *Dispatcher) Run(ctx context.Context) {
	sub := di.Deps.Redis.PSubscribe(ctx, availablePattern)
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
func (di *Dispatcher) handle(ctx context.Context, msg redis.XMessage) {
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
		}
		if next == "0-0" || len(msgs) == 0 {
			return
		}
		start = next
	}
}
