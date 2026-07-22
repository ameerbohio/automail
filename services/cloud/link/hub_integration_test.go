package link

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// TestHub_RegisterSeedsStateAndDispatchReachesSocket exercises the parts
// of the Phase 3 "Verify" line that don't require Postgres: a printer
// dials in, registers, the cloud server seeds mailbox:<id>:state in
// Redis, and a message published to mailbox:<id>:dispatch reaches the
// printer's socket as a dispatch frame.
//
// The Postgres-touching half (status frame -> UpdateJobStatus -> audit
// event) is exercised by sqlc's generated query plus a docker-compose
// run rather than here -- this package has no DB fixture, matching this
// repo's existing test depth (no integration tests touch a live
// Postgres anywhere yet).
func TestHub_RegisterSeedsStateAndDispatchReachesSocket(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	hub := NewHub(rdb, nil) // Queries unused by register/state path

	// hub.Accept upgrades to a WebSocket, which HIJACKS the connection --
	// httptest.Server.Close() does not wait for hijacked conns, so this handler
	// goroutine can outlive the test. Calling t.Logf from it would then race with
	// testing's own teardown (a real, intermittent `-race` failure). Stash the
	// error instead and report it from the test goroutine below.
	acceptErr := make(chan error, 1)
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := hub.Accept(r.Context(), w, r); err != nil {
			select {
			case acceptErr <- err:
			default: // only the first error is interesting
			}
		}
	}))
	defer httpSrv.Close()
	// Runs before httpSrv.Close() (defers are LIFO), but either way this only
	// touches t from the test goroutine, while the test is still alive.
	defer func() {
		select {
		case err := <-acceptErr:
			t.Logf("hub.Accept returned: %v", err)
		default:
		}
	}()

	wsURL := "ws" + httpSrv.URL[len("http"):]

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	mailboxID := "11111111-1111-1111-1111-111111111111"
	if err := wsjson.Write(ctx, conn, Frame{
		Type:          "register",
		MailboxID:     mailboxID,
		SlotOccupancy: map[string]SlotState{"slot-1": {Current: 0, Max: 5}},
	}); err != nil {
		t.Fatalf("write register frame: %v", err)
	}

	// Give the handler goroutine a moment to process the register frame
	// and seed Redis before asserting on it.
	deadline := time.Now().Add(2 * time.Second)
	var raw string
	for time.Now().Before(deadline) {
		raw, err = rdb.Get(ctx, "mailbox:"+mailboxID+":state").Result()
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("mailbox:%s:state never appeared in Redis: %v", mailboxID, err)
	}
	if raw == "" {
		t.Fatal("expected non-empty state cache value")
	}
	t.Logf("seeded state: %s", raw)

	// Now prove cross-node fan-in: publish a dispatch payload to the
	// channel the hub subscribed to on this mailbox's behalf, and check
	// it arrives down the printer's socket as a dispatch frame.
	dispatchPayload := `{"type":"dispatch","job_id":"job-42","encrypted_key":"abc","blob_url":"https://example.invalid/blob"}`
	receivers, err := rdb.Publish(ctx, "mailbox:"+mailboxID+":dispatch", dispatchPayload).Result()
	if err != nil {
		t.Fatalf("publish dispatch: %v", err)
	}
	if receivers == 0 {
		t.Fatal("expected the hub to be subscribed to the mailbox's dispatch channel")
	}

	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	var got Frame
	if err := wsjson.Read(readCtx, conn, &got); err != nil {
		t.Fatalf("read dispatch frame: %v", err)
	}
	if got.Type != "dispatch" || got.JobID != "job-42" {
		t.Fatalf("got %+v, want dispatch frame for job-42", got)
	}
}
