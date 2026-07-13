//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"automail/cloud/dispatch"

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
		psub := subscriberConn.PSubscribe(ctx, "mailbox:*:available")
		defer psub.Close()
		if _, err := psub.Receive(ctx); err != nil {
			t.Fatalf("psubscribe confirm: %v", err)
		}
		ch := psub.Channel()

		mailboxID := uuid.NewString()
		if err := publisher.Publish(ctx, "mailbox:"+mailboxID+":available", "1").Err(); err != nil {
			t.Fatalf("publish available: %v", err)
		}
		select {
		case msg := <-ch:
			if msg.Channel != "mailbox:"+mailboxID+":available" {
				t.Fatalf("channel = %q, want the available channel for %s", msg.Channel, mailboxID)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no message on cross-connection PSUBSCRIBE within 5s")
		}
	})
}
