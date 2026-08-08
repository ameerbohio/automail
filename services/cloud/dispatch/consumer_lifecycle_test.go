package dispatch

// Consumer-group lifecycle tests (plans/16-kubernetes.md §4.2). The group is
// supposed to name the *live* nodes; before this, every container that ever
// ran left an entry behind (4 consumers were measured for 3 live nodes), and a
// Kubernetes Deployment leaks one per pod per rollout.
//
// The trap these tests exist to pin: XGROUP DELCONSUMER *discards* the
// consumer's pending-entries list rather than handing those entries back for
// XAUTOCLAIM. So "delete the consumer on shutdown" is only safe for a consumer
// that owes nothing -- otherwise tidying the group silently deletes jobs.

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// newTestDispatcher wires a Dispatcher to a fresh miniredis with the
// jobs:pending group already created.
func newTestDispatcher(t *testing.T, nodeID string) (*Dispatcher, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	di := &Dispatcher{Deps: Deps{Redis: rdb}, NodeID: nodeID, SweepInt: time.Hour}
	if err := di.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	return di, mr, rdb
}

// seedPendingFor makes name an existing consumer owning one un-ACK'd entry:
// XADD a job, then read it with ">" as that consumer, which is exactly how a
// real node acquires a PEL entry.
func seedPendingFor(t *testing.T, rdb *redis.Client, name string) {
	t.Helper()
	ctx := context.Background()
	ref := JobRef{
		JobID:        uuid.New().String(),
		MailboxID:    "mailbox-1",
		SlotID:       "slot-1",
		EncryptedKey: "YWJj",
		BlobRef:      "blobs/x",
	}
	if _, err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: PendingStream, Values: ref.toXAddValues()}).Result(); err != nil {
		t.Fatalf("seed XAdd: %v", err)
	}
	if _, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    ConsumerGroup,
		Consumer: name,
		Streams:  []string{PendingStream, ">"},
		Count:    1,
		Block:    -1,
	}).Result(); err != nil {
		t.Fatalf("seed XReadGroup for %s: %v", name, err)
	}
}

func consumerNames(t *testing.T, rdb *redis.Client) []string {
	t.Helper()
	infos, err := rdb.XInfoConsumers(context.Background(), PendingStream, ConsumerGroup).Result()
	if err != nil {
		t.Fatalf("XInfoConsumers: %v", err)
	}
	names := make([]string, 0, len(infos))
	for _, i := range infos {
		names = append(names, i.Name)
	}
	return names
}

// TestRemoveConsumer_DeletesDrainedConsumer is the graceful-shutdown case:
// this node read a message, ACK'd it, and is now leaving. Its consumer must
// not survive it.
func TestRemoveConsumer_DeletesDrainedConsumer(t *testing.T) {
	di, _, rdb := newTestDispatcher(t, "node-a")
	ctx := context.Background()

	seedPendingFor(t, rdb, "node-a")
	pending, err := rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: PendingStream, Group: ConsumerGroup, Start: "-", End: "+", Count: 10,
	}).Result()
	if err != nil {
		t.Fatalf("XPendingExt: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("setup: got %d pending entries, want 1", len(pending))
	}
	if err := rdb.XAck(ctx, PendingStream, ConsumerGroup, pending[0].ID).Err(); err != nil {
		t.Fatalf("XAck: %v", err)
	}

	if err := di.RemoveConsumer(ctx); err != nil {
		t.Fatalf("RemoveConsumer: %v", err)
	}
	if names := consumerNames(t, rdb); len(names) != 0 {
		t.Fatalf("consumers after graceful shutdown = %v, want none", names)
	}
}

// TestRemoveConsumer_KeepsConsumerHoldingPendingEntries is the trap. Deleting
// a consumer that still owns un-ACK'd entries would discard them -- the job is
// gone, not reclaimable -- so the tidy-up must decline. The consumer staying
// is the *correct* outcome, and the reaper picks it up later once reclaim has
// emptied its PEL.
func TestRemoveConsumer_KeepsConsumerHoldingPendingEntries(t *testing.T) {
	di, _, rdb := newTestDispatcher(t, "node-a")
	ctx := context.Background()

	seedPendingFor(t, rdb, "node-a") // read but never ACK'd

	if err := di.RemoveConsumer(ctx); err != nil {
		t.Fatalf("RemoveConsumer: %v", err)
	}
	names := consumerNames(t, rdb)
	if len(names) != 1 || names[0] != "node-a" {
		t.Fatalf("consumers = %v, want [node-a] kept (DELCONSUMER would have discarded its PEL)", names)
	}
	// And the entry itself must still be pending -- i.e. still reclaimable.
	pending, err := rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: PendingStream, Group: ConsumerGroup, Start: "-", End: "+", Count: 10,
	}).Result()
	if err != nil {
		t.Fatalf("XPendingExt: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending entries after refused delete = %d, want 1 (the job must stay reclaimable)", len(pending))
	}
}

// TestReapStaleConsumers_SparesLiveConsumers covers the reaper's cheap half
// here; the interesting half is deliberately elsewhere.
//
// miniredis reports XINFO CONSUMERS `idle: -1` (it never records a consumer's
// last-seen time for XREADGROUP), so the idle *threshold* -- the whole point
// of the reaper -- cannot be exercised against the fake at all. Asserting it
// here would only prove the fake's placeholder. The real behaviour is pinned
// against real Redis in TestIntegration_ReapsStaleConsumers
// (`make test-integration`), with the threshold shortened via ReapIdle.
//
// What this does pin is that nothing is reaped when nothing looks dead --
// including this node's own consumer, which is live by definition.
func TestReapStaleConsumers_SparesLiveConsumers(t *testing.T) {
	di, _, rdb := newTestDispatcher(t, "node-live")
	ctx := context.Background()

	seedPendingFor(t, rdb, "node-other")
	pending, err := rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: PendingStream, Group: ConsumerGroup, Start: "-", End: "+", Count: 10, Consumer: "node-other",
	}).Result()
	if err != nil {
		t.Fatalf("XPendingExt: %v", err)
	}
	if err := rdb.XAck(ctx, PendingStream, ConsumerGroup, pending[0].ID).Err(); err != nil {
		t.Fatalf("XAck: %v", err)
	}
	seedPendingFor(t, rdb, "node-live")

	di.ReapStaleConsumers(ctx)

	got := map[string]bool{}
	for _, n := range consumerNames(t, rdb) {
		got[n] = true
	}
	if !got["node-other"] || !got["node-live"] {
		t.Fatalf("consumers = %v, want both kept (neither is idle past the threshold)", consumerNames(t, rdb))
	}
}

// TestReapMinIdleExceedsClaimWindow pins the relationship the reaper's
// safety argument rests on: a consumer must never be reaped while XAUTOCLAIM
// is still the expected recovery path for its entries.
func TestReapMinIdleExceedsClaimWindow(t *testing.T) {
	if reapMinIdle <= claimMinIdle {
		t.Fatalf("reapMinIdle (%s) must exceed claimMinIdle (%s)", reapMinIdle, claimMinIdle)
	}
}

// TestHandleDetachesFromCallerContext pins the §4.1 decision: cancelling the
// Run loop's context stops this node reading new work, but an attempt already
// underway completes. Here the caller's context is already cancelled, yet the
// malformed message still gets ACK'd off the PEL -- had handle inherited that
// context, the XACK would have failed and the entry would sit pending forever.
func TestHandleDetachesFromCallerContext(t *testing.T) {
	di, _, rdb := newTestDispatcher(t, "node-a")

	seedPendingFor(t, rdb, "node-a")
	pending, err := rdb.XPendingExt(context.Background(), &redis.XPendingExtArgs{
		Stream: PendingStream, Group: ConsumerGroup, Start: "-", End: "+", Count: 10,
	}).Result()
	if err != nil {
		t.Fatalf("XPendingExt: %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	// Values without job_id -> JobRefFromValues fails -> handle ACKs it as
	// unretryable. That ACK is the observable side effect of the detach.
	di.handle(cancelled, redis.XMessage{ID: pending[0].ID, Values: map[string]any{"nonsense": "1"}})

	after, err := rdb.XPendingExt(context.Background(), &redis.XPendingExtArgs{
		Stream: PendingStream, Group: ConsumerGroup, Start: "-", End: "+", Count: 10,
	}).Result()
	if err != nil {
		t.Fatalf("XPendingExt after: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("pending after handle on a cancelled context = %d, want 0 (handle must run on a detached context)", len(after))
	}
}
