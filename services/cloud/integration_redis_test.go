//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"automail/cloud/dispatch"
	"automail/cloud/store"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// newStreamMessage is a representative jobs:pending entry (the shape
// dispatch.JobRef.toXAddValues produces), built inline so this test drives
// the real stream field set without reaching into unexported helpers.
func newStreamMessage() map[string]any {
	return map[string]any{
		"job_id":        uuid.NewString(),
		"mailbox_id":    uuid.NewString(),
		"slot_id":       uuid.NewString(),
		"encrypted_key": "YWJj",
		"blob_ref":      "blobs/x",
	}
}

// TestIntegration_StreamConsumerGroupRoundTrip exercises the full
// XADD -> XREADGROUP(">") -> XACK cycle against real Redis Streams using
// the production stream/group names and the same EnsureGroup call main.go
// makes at startup. A miniredis fake implements these commands, but only
// real Redis proves the consumer-group delivery + PEL bookkeeping the
// dispatcher relies on: after XACK the Pending Entries List is empty.
func TestIntegration_StreamConsumerGroupRoundTrip(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()

	di := &dispatch.Dispatcher{Deps: dispatch.Deps{Redis: rdb}, NodeID: "node-A"}
	// EnsureGroup at "$" only sees entries added afterwards -- production
	// creates the group at startup before jobs arrive, so create then add.
	if err := di.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	id, err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: dispatch.PendingStream,
		Values: newStreamMessage(),
	}).Result()
	if err != nil {
		t.Fatalf("XAdd: %v", err)
	}

	streams, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    dispatch.ConsumerGroup,
		Consumer: "node-A",
		Streams:  []string{dispatch.PendingStream, ">"},
		Count:    10,
		Block:    -1,
	}).Result()
	if err != nil {
		t.Fatalf("XReadGroup: %v", err)
	}
	if len(streams) != 1 || len(streams[0].Messages) != 1 {
		t.Fatalf("XReadGroup returned %d streams; want 1 message", len(streams))
	}
	if got := streams[0].Messages[0].ID; got != id {
		t.Fatalf("read message ID = %s, want %s", got, id)
	}

	// Before ACK the entry is pending for this consumer.
	pending, err := rdb.XPending(ctx, dispatch.PendingStream, dispatch.ConsumerGroup).Result()
	if err != nil {
		t.Fatalf("XPending (pre-ack): %v", err)
	}
	if pending.Count != 1 {
		t.Fatalf("pending count pre-ack = %d, want 1", pending.Count)
	}

	if err := rdb.XAck(ctx, dispatch.PendingStream, dispatch.ConsumerGroup, id).Err(); err != nil {
		t.Fatalf("XAck: %v", err)
	}

	// After ACK the PEL is empty -- the message is fully processed.
	pending, err = rdb.XPending(ctx, dispatch.PendingStream, dispatch.ConsumerGroup).Result()
	if err != nil {
		t.Fatalf("XPending (post-ack): %v", err)
	}
	if pending.Count != 0 {
		t.Fatalf("pending count post-ack = %d, want 0", pending.Count)
	}
}

// TestIntegration_XAutoClaimReclaims is the crash-recovery path
// (plans/03-scaling.md, dispatcher.reclaim): node-A reads a message but
// never ACKs it (simulating a crash mid-dispatch). node-B then reclaims it
// via XAUTOCLAIM and can ACK it, so the job is not stranded. miniredis's
// XAUTOCLAIM support is partial; this proves the real semantics the
// failover design depends on.
func TestIntegration_XAutoClaimReclaims(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()

	di := &dispatch.Dispatcher{Deps: dispatch.Deps{Redis: rdb}, NodeID: "node-A"}
	if err := di.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	id, err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: dispatch.PendingStream,
		Values: newStreamMessage(),
	}).Result()
	if err != nil {
		t.Fatalf("XAdd: %v", err)
	}

	// node-A reads it into its PEL but "crashes" before ACK.
	if _, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    dispatch.ConsumerGroup,
		Consumer: "node-A",
		Streams:  []string{dispatch.PendingStream, ">"},
		Count:    10,
		Block:    -1,
	}).Result(); err != nil {
		t.Fatalf("node-A XReadGroup: %v", err)
	}

	// node-B reclaims anything idle >= 0 (MinIdle 0 so we don't wait).
	msgs, _, err := rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   dispatch.PendingStream,
		Group:    dispatch.ConsumerGroup,
		Consumer: "node-B",
		MinIdle:  0,
		Start:    "0-0",
		Count:    10,
	}).Result()
	if err != nil {
		t.Fatalf("XAutoClaim: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != id {
		t.Fatalf("XAutoClaim reclaimed %d messages; want the 1 un-ACKed entry %s", len(msgs), id)
	}

	// node-B now owns it and can ACK it -- the job is recovered, not lost.
	if err := rdb.XAck(ctx, dispatch.PendingStream, dispatch.ConsumerGroup, id).Err(); err != nil {
		t.Fatalf("node-B XAck: %v", err)
	}
	pending, err := rdb.XPending(ctx, dispatch.PendingStream, dispatch.ConsumerGroup).Result()
	if err != nil {
		t.Fatalf("XPending post-reclaim-ack: %v", err)
	}
	if pending.Count != 0 {
		t.Fatalf("pending count after reclaim+ack = %d, want 0", pending.Count)
	}
}

// TestIntegration_PubSubCrossConnection proves the cross-node fan-out the
// dispatch design depends on: a message PUBLISHed on one connection reaches
// a SUBSCRIBE / PSUBSCRIBE subscriber on a *different* connection. In
// production the dispatcher goroutine (dispatcher.Run, PSUBSCRIBE
// mailbox:*:available) and the owner node holding the printer socket
// (attemptDispatch, PUBLISH mailbox:<id>:dispatch) are different processes;
// this is the Redis behavior that lets a job claimed on a non-owner node
// still reach the owner.
func TestIntegration_PubSubCrossConnection(t *testing.T) {
	publisher := startRedis(t)
	ctx := context.Background()

	// A genuinely separate connection to the same server -- the subscriber
	// must not share the publisher's connection.
	subscriberConn := redis.NewClient(&redis.Options{Addr: publisher.Options().Addr})
	defer subscriberConn.Close()

	t.Run("exact channel SUBSCRIBE", func(t *testing.T) {
		channel := uniqueName("mailbox:disp")
		sub := subscriberConn.Subscribe(ctx, channel)
		defer sub.Close()
		// Wait for the subscription to be established before publishing.
		if _, err := sub.Receive(ctx); err != nil {
			t.Fatalf("subscribe confirm: %v", err)
		}
		ch := sub.Channel()

		if err := publisher.Publish(ctx, channel, "dispatch-frame").Err(); err != nil {
			t.Fatalf("publish: %v", err)
		}
		select {
		case msg := <-ch:
			if msg.Payload != "dispatch-frame" {
				t.Fatalf("payload = %q, want %q", msg.Payload, "dispatch-frame")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no message on cross-connection SUBSCRIBE within 5s")
		}
	})

	t.Run("pattern PSUBSCRIBE", func(t *testing.T) {
		// Mirrors dispatcher.availablePattern: the dispatcher subscribes to
		// every mailbox's availability channel without knowing the IDs.
		psub := subscriberConn.PSubscribe(ctx, store.PatternAvailable)
		defer psub.Close()
		if _, err := psub.Receive(ctx); err != nil {
			t.Fatalf("psubscribe confirm: %v", err)
		}
		ch := psub.Channel()

		mailboxID := uuid.NewString()
		if err := publisher.Publish(ctx, store.ChanAvailable(mailboxID), "1").Err(); err != nil {
			t.Fatalf("publish available: %v", err)
		}
		select {
		case msg := <-ch:
			if msg.Channel != store.ChanAvailable(mailboxID) {
				t.Fatalf("channel = %q, want the available channel for %s", msg.Channel, mailboxID)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no message on cross-connection PSUBSCRIBE within 5s")
		}
	})
}

// TestIntegration_ReapsStaleConsumers pins the consumer-group lifecycle
// against real Redis (plans/16-kubernetes.md §4.2). It has to run here rather
// than as a unit test because miniredis does not track a consumer's last-seen
// time: it reports XINFO CONSUMERS `idle: -1` for every consumer, so the idle
// threshold the reaper is built around is meaningless against the fake.
//
// Three properties, one run, since they are only safe together:
//   - a consumer idle past the threshold with an empty PEL is deleted (the
//     OOMKill / lost-node case that graceful DELCONSUMER can never cover);
//   - a consumer idle past the threshold that still owns un-ACK'd entries is
//     KEPT -- XGROUP DELCONSUMER discards a PEL instead of handing it back, so
//     reaping it would destroy a job XAUTOCLAIM was about to reclaim;
//   - the reaping node never deletes its own consumer.
func TestIntegration_ReapsStaleConsumers(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()

	// ReapIdle shortens the real 5-minute threshold; everything else is the
	// production path.
	di := &dispatch.Dispatcher{
		Deps:     dispatch.Deps{Redis: rdb},
		NodeID:   "node-live",
		ReapIdle: 300 * time.Millisecond,
	}
	if err := di.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	// One entry per consumer, so each of them exists in the group.
	read := func(consumer string) string {
		t.Helper()
		id, err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: dispatch.PendingStream, Values: newStreamMessage()}).Result()
		if err != nil {
			t.Fatalf("XAdd: %v", err)
		}
		if _, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    dispatch.ConsumerGroup,
			Consumer: consumer,
			Streams:  []string{dispatch.PendingStream, ">"},
			Count:    10,
			Block:    -1,
		}).Result(); err != nil {
			t.Fatalf("%s XReadGroup: %v", consumer, err)
		}
		return id
	}

	drainedID := read("node-drained") // dead, owes nothing
	read("node-holding")              // dead, still owes an entry
	liveID := read("node-live")       // this node

	for _, id := range []string{drainedID, liveID} {
		if err := rdb.XAck(ctx, dispatch.PendingStream, dispatch.ConsumerGroup, id).Err(); err != nil {
			t.Fatalf("XAck %s: %v", id, err)
		}
	}

	// Let every consumer's idle clock pass ReapIdle. node-live's does too --
	// the self-skip must not depend on it looking busy.
	time.Sleep(500 * time.Millisecond)

	di.ReapStaleConsumers(ctx)

	infos, err := rdb.XInfoConsumers(ctx, dispatch.PendingStream, dispatch.ConsumerGroup).Result()
	if err != nil {
		t.Fatalf("XInfoConsumers: %v", err)
	}
	left := map[string]bool{}
	for _, i := range infos {
		left[i.Name] = true
	}
	if left["node-drained"] {
		t.Error("node-drained was idle past the threshold with an empty PEL; it should have been reaped")
	}
	if !left["node-holding"] {
		t.Error("node-holding still owns an un-ACKed entry; reaping it would discard that job (DELCONSUMER drops the PEL)")
	}
	if !left["node-live"] {
		t.Error("the reaper deleted its own consumer")
	}

	// The kept consumer's entry must still be pending -- i.e. still
	// reclaimable by XAUTOCLAIM, which is the whole reason it was spared.
	pending, err := rdb.XPending(ctx, dispatch.PendingStream, dispatch.ConsumerGroup).Result()
	if err != nil {
		t.Fatalf("XPending: %v", err)
	}
	if pending.Count != 1 {
		t.Fatalf("pending entries after reap = %d, want 1 (node-holding's job survives)", pending.Count)
	}
}

// TestIntegration_RemoveConsumerOnShutdown is the graceful half of the same
// contract, against real Redis: after a node drains and leaves, the group
// names only the nodes that are still running. This is what stops the group
// growing by one entry per pod per rollout.
func TestIntegration_RemoveConsumerOnShutdown(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()

	leaving := &dispatch.Dispatcher{Deps: dispatch.Deps{Redis: rdb}, NodeID: "node-leaving"}
	if err := leaving.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	id, err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: dispatch.PendingStream, Values: newStreamMessage()}).Result()
	if err != nil {
		t.Fatalf("XAdd: %v", err)
	}
	if _, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    dispatch.ConsumerGroup,
		Consumer: "node-leaving",
		Streams:  []string{dispatch.PendingStream, ">"},
		Count:    10,
		Block:    -1,
	}).Result(); err != nil {
		t.Fatalf("XReadGroup: %v", err)
	}

	// Still holding the entry: the shutdown path must refuse to delete.
	if err := leaving.RemoveConsumer(ctx); err != nil {
		t.Fatalf("RemoveConsumer (holding): %v", err)
	}
	infos, err := rdb.XInfoConsumers(ctx, dispatch.PendingStream, dispatch.ConsumerGroup).Result()
	if err != nil {
		t.Fatalf("XInfoConsumers: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("consumers while holding a pending entry = %d, want 1 (delete must be refused)", len(infos))
	}

	// Drained: now it goes.
	if err := rdb.XAck(ctx, dispatch.PendingStream, dispatch.ConsumerGroup, id).Err(); err != nil {
		t.Fatalf("XAck: %v", err)
	}
	if err := leaving.RemoveConsumer(ctx); err != nil {
		t.Fatalf("RemoveConsumer (drained): %v", err)
	}
	infos, err = rdb.XInfoConsumers(ctx, dispatch.PendingStream, dispatch.ConsumerGroup).Result()
	if err != nil {
		t.Fatalf("XInfoConsumers: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("consumers after a graceful shutdown = %d, want 0", len(infos))
	}
}
