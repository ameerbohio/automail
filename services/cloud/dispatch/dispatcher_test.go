package dispatch

import (
	"context"
	"testing"
	"time"

	"automail/cloud/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// TestDispatcher_BlockedRetryStaysPendingWithoutDuplicating exercises the
// Phase 4 "Verify" line's queue half through Redis: a job XADD'd onto
// jobs:pending sits there until a mailbox:<id>:available event fires, the
// dispatcher's consumer-group read picks it up, and retries via Retry
// (not TryDispatch -- see route.go's doc comment on why those differ).
// Since no printer state is ever cached in this test, the job stays
// permanently ineligible; the assertion that matters is that the retry
// leaves the *same* message pending instead of XADD-ing a duplicate or
// silently dropping it (Retry must never call enqueue).
//
// This does not touch Postgres -- attemptDispatch's not-eligible branch
// returns before claimJob ever runs (the same property
// TestTryDispatch_NotEligibleEnqueues in route_test.go checks directly),
// so Deps.SQLDB and Deps.Queries are left nil here, matching this
// package's existing no-Postgres-fixture test depth (see
// link/hub_integration_test.go).
func TestDispatcher_BlockedRetryStaysPendingWithoutDuplicating(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	ctx := context.Background()

	deps := Deps{Redis: rdb}
	di := &Dispatcher{Deps: deps, NodeID: "test-node", SweepInt: time.Hour} // sweep disabled for this test
	if err := di.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	const mailboxID = "mailbox-1"
	ref := JobRef{
		JobID:        uuid.New().String(),
		MailboxID:    mailboxID,
		SlotID:       "slot-1",
		EncryptedKey: "YWJj",
		BlobRef:      "blobs/x",
	}
	if _, err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: PendingStream, Values: ref.toXAddValues()}).Result(); err != nil {
		t.Fatalf("seed XAdd: %v", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go di.Run(runCtx)

	// Give Run's PSUBSCRIBE a moment to attach before publishing --
	// otherwise the event could fire before anyone's listening.
	time.Sleep(100 * time.Millisecond)

	receivers, err := rdb.Publish(ctx, store.ChanAvailable(mailboxID), "1").Result()
	if err != nil {
		t.Fatalf("publish available: %v", err)
	}
	if receivers == 0 {
		t.Fatal("expected the dispatcher to be PSUBSCRIBEd to mailbox:*:available")
	}

	// Give the goroutine time to process the available event via drain.
	// Whether or not it ran, exactly one stream entry must exist the
	// whole time -- Retry must never XADD a duplicate for a job that's
	// still blocked.
	time.Sleep(300 * time.Millisecond)

	raw, err := rdb.XRange(ctx, PendingStream, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRange: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("got %d stream entries, want exactly 1 (Retry must not duplicate a still-blocked job)", len(raw))
	}

	// The message must still be in the PEL (un-ACK'd): a blocked retry
	// leaves it for the next available event or XAUTOCLAIM sweep, it does
	// not drop it.
	pending, err := rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: PendingStream,
		Group:  ConsumerGroup,
		Start:  "-",
		End:    "+",
		Count:  10,
	}).Result()
	if err != nil {
		t.Fatalf("XPendingExt: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending entries, want 1 (still-blocked job left un-ACK'd)", len(pending))
	}
}

// TestDispatcher_DrainReturnsOnEmptyStream pins drain's non-blocking
// contract: with nothing in jobs:pending it must return to Run's select
// loop immediately. A blocking XREADGROUP here (BLOCK 0 = wait forever;
// go-redis sends BLOCK for any Block >= 0, so only a negative Block omits
// it) would pin the dispatcher goroutine inside drain and starve the
// available-event and XAUTOCLAIM sweep cases forever.
func TestDispatcher_DrainReturnsOnEmptyStream(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	ctx := context.Background()

	di := &Dispatcher{Deps: Deps{Redis: rdb}, NodeID: "test-node"}
	if err := di.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	done := make(chan struct{})
	go func() {
		di.drain(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drain blocked on an empty stream; XREADGROUP must not send BLOCK")
	}
}

// TestDispatcher_EnsureGroupIsIdempotent confirms calling EnsureGroup
// twice (every node does this on startup, and a node can restart) doesn't
// error on the second call -- BUSYGROUP is the expected, swallowed case.
func TestDispatcher_EnsureGroupIdempotent(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	ctx := context.Background()

	di := &Dispatcher{Deps: Deps{Redis: rdb}, NodeID: "test-node"}
	if err := di.EnsureGroup(ctx); err != nil {
		t.Fatalf("first EnsureGroup: %v", err)
	}
	if err := di.EnsureGroup(ctx); err != nil {
		t.Fatalf("second EnsureGroup (should be a no-op BUSYGROUP): %v", err)
	}
}
