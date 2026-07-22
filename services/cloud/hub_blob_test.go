package main

// Phase 6 test for the cloud side of the real blob lifecycle: when the
// printer reports "delivered", the hub deletes the spent ciphertext from
// object storage and records it (plans/05-cloud-server.md "On delivered:
// request MinIO blob deletion"). Driven end-to-end through hub.Accept over
// a real WebSocket, like the cross-node fan-out test; Postgres is the fake
// driver (dbfake_test.go), Redis is miniredis, MinIO is a fake DeleteBlob.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"automail/cloud/db"
	"automail/cloud/link"
	"automail/cloud/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// blobDeleteQueries serves the queries onStatus + deleteDeliveredBlob issue.
// It signals blobDeleted once the blob_deleted audit event is written (the
// last step of a successful deletion), and flips setDeleted when
// SetJobBlobDeleted runs.
func blobDeleteQueries(jobID, mailboxID, blobRef string, setDeleted *atomic.Bool, blobDeleted chan<- struct{}) fakeQueryFunc {
	return func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		switch {
		case strings.HasPrefix(query, "-- name: UpdateJobStatus"):
			status, _ := args[0].Value.(string)
			return []string{"id", "mailbox_id", "blob_ref", "status"},
				[][]driver.Value{{jobID, mailboxID, blobRef, status}}, nil
		case strings.HasPrefix(query, "-- name: SetJobBlobDeleted"):
			setDeleted.Store(true)
			return nil, nil, nil
		case strings.HasPrefix(query, "-- name: InsertAuditEvent"):
			if len(args) >= 2 {
				if action, _ := args[1].Value.(string); action == "blob_deleted" {
					select {
					case blobDeleted <- struct{}{}:
					default:
					}
				}
			}
			return nil, nil, nil
		default:
			return nil, nil, fmt.Errorf("unexpected query: %.60s", query)
		}
	}
}

func TestHubDeletesBlobOnDelivered(t *testing.T) {
	mr := miniredis.RunT(t)
	jobID := uuid.New().String()
	mailboxID := uuid.New().String()
	const blobRef = "blobs/deliver-me"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var setDeleted atomic.Bool
	blobDeleted := make(chan struct{}, 1)
	sqlDB := sql.OpenDB(fakeConnector{q: blobDeleteQueries(jobID, mailboxID, blobRef, &setDeleted, blobDeleted)})
	defer sqlDB.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	hub := link.NewHub(rdb, db.New(sqlDB))
	var deleteCalls atomic.Int32
	gotRef := make(chan string, 1)
	hub.DeleteBlob = func(_ context.Context, ref string) error {
		deleteCalls.Add(1)
		select {
		case gotRef <- ref:
		default:
		}
		return nil
	}

	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := hub.Accept(r.Context(), w, r); err != nil {
			t.Logf("hub.Accept returned: %v", err)
		}
	}))
	defer node.Close()

	printer, _, err := websocket.Dial(ctx, "ws"+node.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("printer dial: %v", err)
	}
	defer printer.CloseNow()
	if err := wsjson.Write(ctx, printer, link.Frame{
		Type:          "register",
		MailboxID:     mailboxID,
		SlotOccupancy: map[string]link.SlotState{"slot-1": {Current: 0, Max: 5}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// "printing" must NOT trigger a blob delete...
	if err := wsjson.Write(ctx, printer, link.Frame{Type: "status", JobID: jobID, Status: "printing"}); err != nil {
		t.Fatalf("printing frame: %v", err)
	}
	// ...only "delivered" does.
	if err := wsjson.Write(ctx, printer, link.Frame{Type: "status", JobID: jobID, Status: "delivered"}); err != nil {
		t.Fatalf("delivered frame: %v", err)
	}

	select {
	case <-blobDeleted:
	case <-ctx.Done():
		t.Fatal("timed out waiting for blob_deleted audit event")
	}

	if got := deleteCalls.Load(); got != 1 {
		t.Fatalf("DeleteBlob called %d times, want exactly 1 (printing must not delete)", got)
	}
	select {
	case ref := <-gotRef:
		if ref != blobRef {
			t.Fatalf("DeleteBlob got blob_ref %q, want %q", ref, blobRef)
		}
	default:
		t.Fatal("DeleteBlob recorded no blob_ref")
	}
	if !setDeleted.Load() {
		t.Fatal("SetJobBlobDeleted was not called after a successful blob delete")
	}
}

// TestHubDeliveredWithoutDeleteBlobDoesNotPanic guards the nil-DeleteBlob
// path (a hub built without the injection): delivery must still succeed,
// and no SetJobBlobDeleted/blob_deleted audit should run.
func TestHubDeliveredWithoutDeleteBlobDoesNotPanic(t *testing.T) {
	mr := miniredis.RunT(t)
	jobID := uuid.New().String()
	mailboxID := uuid.New().String()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var setDeleted atomic.Bool
	// Fail the test if SetJobBlobDeleted is ever reached with no DeleteBlob.
	q := func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if strings.HasPrefix(query, "-- name: SetJobBlobDeleted") {
			setDeleted.Store(true)
		}
		return blobDeleteQueries(jobID, mailboxID, "blobs/x", &setDeleted, make(chan struct{}, 1))(query, args)
	}
	sqlDB := sql.OpenDB(fakeConnector{q: q})
	defer sqlDB.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	hub := link.NewHub(rdb, db.New(sqlDB)) // DeleteBlob left nil

	// Subscribe to the status channel so we can confirm delivery was relayed
	// even though no blob deletion happens.
	sub := rdb.Subscribe(ctx, store.ChanJobStatus(jobID))
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	ch := sub.Channel()

	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := hub.Accept(r.Context(), w, r); err != nil {
			t.Logf("hub.Accept returned: %v", err)
		}
	}))
	defer node.Close()

	printer, _, err := websocket.Dial(ctx, "ws"+node.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("printer dial: %v", err)
	}
	defer printer.CloseNow()
	if err := wsjson.Write(ctx, printer, link.Frame{Type: "register", MailboxID: mailboxID}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := wsjson.Write(ctx, printer, link.Frame{Type: "status", JobID: jobID, Status: "delivered"}); err != nil {
		t.Fatalf("delivered frame: %v", err)
	}

	select {
	case <-ch: // delivery relayed to SSE despite no blob deletion
	case <-ctx.Done():
		t.Fatal("timed out waiting for the delivered status to be relayed")
	}
	if setDeleted.Load() {
		t.Fatal("SetJobBlobDeleted ran even though DeleteBlob was nil")
	}
}
