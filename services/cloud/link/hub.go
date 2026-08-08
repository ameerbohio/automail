package link

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"sync"

	"automail/cloud/db"
	"automail/cloud/store"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Hub owns the printer-link upgrade handler and the read loop for every
// connected printer on this node. It is the cloud-server-side half of
// plans/04-printer-microservice.md's "Connection lifecycle".
type Hub struct {
	Registry *Registry
	Redis    *redis.Client
	Queries  *db.Queries
	// DeleteBlob removes a delivered job's ciphertext from object storage.
	// Injected (real MinIO RemoveObject in main; a fake in tests) so the hub
	// stays testable without a live MinIO. A nil DeleteBlob disables
	// deletion -- the blob is left for a TTL/sweep to reclaim -- so older
	// callers that predate the blob lifecycle keep working unchanged. It is
	// handed only blob_ref; encrypted_key never reaches this path.
	DeleteBlob func(ctx context.Context, blobRef string) error
}

func NewHub(rdb *redis.Client, queries *db.Queries) *Hub {
	return &Hub{
		Registry: NewRegistry(),
		Redis:    rdb,
		Queries:  queries,
	}
}

// Accept upgrades an already mTLS-verified HTTP request to a WebSocket,
// reads the initial register frame, then runs the link loop until the
// socket closes. Intended to run in the request goroutine of
// GET /internal/printer-link -- it blocks for the life of the connection.
func (h *Hub) Accept(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()

	var reg Frame
	if err := wsjson.Read(ctx, conn, &reg); err != nil {
		return err
	}
	if reg.Type != "register" || reg.MailboxID == "" {
		conn.Close(websocket.StatusPolicyViolation, "expected register frame")
		return errors.New("printer-link: first frame was not a valid register frame")
	}

	log.Printf("printer-link: mailbox %s registered (slots: %v)", reg.MailboxID, reg.SlotOccupancy)

	h.Registry.Add(reg.MailboxID, conn)
	defer h.Registry.Remove(reg.MailboxID, conn)

	// The owner node (this one) subscribes so any node's tryDispatch can
	// route a job to this socket via PUBLISH -- see "Why publish instead
	// of call" in plans/05-cloud-server.md. pumpDispatch exits on its own
	// once ctx is cancelled (read loop below returns -> caller cancels).
	//
	// This must happen, and be acked, before the state cache is seeded
	// below: Subscribe() only writes the SUBSCRIBE command, it doesn't wait
	// for the server's reply, so without the Receive a tryDispatch PUBLISH
	// landing right after register could see zero subscribers and wrongly
	// requeue the job. Seeding state only after the ack gives callers a
	// reliable signal to poll on: once mailbox:<id>:state is visible, the
	// dispatch subscription is guaranteed already active.
	pumpCtx, cancelPump := context.WithCancel(ctx)
	defer cancelPump()
	sub := h.Redis.Subscribe(pumpCtx, store.ChanDispatch(reg.MailboxID))
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		log.Printf("printer-link: mailbox %s: dispatch subscribe ack: %v", reg.MailboxID, err)
		return err
	}
	go h.pumpDispatch(pumpCtx, conn, sub)

	if err := store.SetPrinterState(ctx, h.Redis, reg.MailboxID, registerToState(reg)); err != nil {
		log.Printf("printer-link: mailbox %s: seed state cache: %v", reg.MailboxID, err)
	}
	// A freshly registered printer is idle and available. registerToState
	// seeds the same "idle" into the Redis cache above.
	h.mirrorLiveness(ctx, reg.MailboxID, "idle")

	return h.readLoop(ctx, conn, reg.MailboxID)
}

// Close tells every printer connected to this node that the node is going
// away, so it reconnects immediately instead of waiting out a TCP timeout.
//
// This exists because the printer link is a *hijacked* connection:
// http.Server.Shutdown neither waits for hijacked conns nor closes them
// (plans/16-kubernetes.md §4.1), so without this the socket simply dies with
// the process. The printer's dial loop only learns that from a read error --
// which, if the pod's network namespace disappears rather than sending a FIN,
// can take minutes. A StatusGoingAway close frame makes it deliberate: the
// printer sees a clean close and re-dials, landing on a surviving node.
//
// Each Close is bounded internally (the library's close handshake times out
// after 5s) and they run concurrently, but the whole set is also bounded by
// ctx so one wedged socket cannot hold shutdown open.
func (h *Hub) Close(ctx context.Context) {
	conns := h.Registry.Conns()
	if len(conns) == 0 {
		return
	}
	log.Printf("printer-link: closing %d printer socket(s) with going-away", len(conns))

	var wg sync.WaitGroup
	for _, conn := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := conn.Close(websocket.StatusGoingAway, "cloud-server shutting down"); err != nil {
				// The peer never echoed the close frame (or the socket was
				// already broken). Drop it hard rather than leaving a
				// half-closed conn behind.
				_ = conn.CloseNow()
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		log.Printf("printer-link: close handshake did not finish before the shutdown deadline")
	}
}

// readLoop processes state and status frames until the socket errors out
// (including a missed ping/pong), at which point Accept's deferred
// Registry.Remove runs and the Redis state key is left to TTL-expire --
// that expiry, not the closed socket itself, is the system's offline
// signal (plans/05-cloud-server.md).
func (h *Hub) readLoop(ctx context.Context, conn *websocket.Conn, mailboxID string) error {
	for {
		var frame Frame
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			log.Printf("printer-link: mailbox %s: read loop ended: %v", mailboxID, err)
			return err
		}
		switch frame.Type {
		case "state":
			h.onState(ctx, mailboxID, frame)
		case "status":
			h.onStatus(ctx, frame)
		default:
			log.Printf("printer-link: mailbox %s: ignoring unknown frame type %q", mailboxID, frame.Type)
		}
	}
}

func (h *Hub) onState(ctx context.Context, mailboxID string, frame Frame) {
	state := store.PrinterState{Status: frame.Status, SlotOccupancy: toStoreSlots(frame.SlotOccupancy)}
	if err := store.SetPrinterState(ctx, h.Redis, mailboxID, state); err != nil {
		log.Printf("printer-link: mailbox %s: refresh state cache: %v", mailboxID, err)
		return
	}
	h.mirrorLiveness(ctx, mailboxID, frame.Status)
	if frame.Status == "idle" {
		// Wakes the Phase 4 dispatcher goroutine, which re-evaluates the
		// blocked-jobs stream whenever a printer becomes available. No
		// dispatcher subscribes yet in Phase 3 -- PUBLISH with zero
		// subscribers is a harmless no-op.
		h.Redis.Publish(ctx, store.ChanAvailable(mailboxID), "1")
	}
}

// mirrorLiveness best-effort updates the durable ops-dashboard mirror
// (mailboxes.status + last_heartbeat_at, plans/08-data-models.md) from a
// register or state frame. Live routing reads the Redis cache; this row exists
// only so the ops dashboard (Phase 9) can show a real last-heartbeat time, so a
// failed or skipped update is logged, never fatal to the link. A nil Queries
// (the register/state-only test hub) disables it, mirroring the DeleteBlob
// nil-guard.
func (h *Hub) mirrorLiveness(ctx context.Context, mailboxID, status string) {
	if h.Queries == nil {
		return
	}
	id, err := uuid.Parse(mailboxID)
	if err != nil {
		log.Printf("printer-link: mirror liveness: invalid mailbox id %q: %v", mailboxID, err)
		return
	}
	if err := h.Queries.UpdateMailboxLiveness(ctx, db.UpdateMailboxLivenessParams{ID: id, Status: status}); err != nil {
		log.Printf("printer-link: mailbox %s: mirror liveness: %v", mailboxID, err)
	}
}

// onStatus applies a job lifecycle update to Postgres and relays it to any
// sender watching via SSE (Phase 5). On "delivered" it also deletes the
// now-spent ciphertext blob from object storage (Phase 6's real blob
// lifecycle) -- the printer has decrypted and printed it, so the ciphertext
// no longer needs to exist.
func (h *Hub) onStatus(ctx context.Context, frame Frame) {
	jobID, err := uuid.Parse(frame.JobID)
	if err != nil {
		log.Printf("printer-link: status frame with invalid job_id %q: %v", frame.JobID, err)
		return
	}

	job, err := h.Queries.UpdateJobStatus(ctx, db.UpdateJobStatusParams{ID: jobID, Status: frame.Status})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			log.Printf("printer-link: status frame for unknown job %s", frame.JobID)
			return
		}
		log.Printf("printer-link: update job %s status: %v", frame.JobID, err)
		return
	}

	action := "job_" + frame.Status // job_printing | job_delivered | job_failed
	if err := h.Queries.InsertAuditEvent(ctx, db.InsertAuditEventParams{JobID: jobID, Action: action}); err != nil {
		log.Printf("printer-link: write audit event for job %s: %v", frame.JobID, err)
	}

	payload, _ := jsonStatusPayload(frame)
	h.Redis.Publish(ctx, store.ChanJobStatus(jobID.String()), payload)

	// Delete the ciphertext AFTER the SSE publish so a slow or failed delete
	// never delays the sender's status update. Keyed on job.BlobRef only --
	// the zero-knowledge boundary is preserved (no encrypted_key here).
	if frame.Status == "delivered" {
		h.deleteDeliveredBlob(ctx, jobID, job.BlobRef)
	}

	log.Printf("printer-link: job %s -> %s (mailbox %s)", job.ID, job.Status, job.MailboxID)
}

// deleteDeliveredBlob removes a delivered job's ciphertext and records the
// deletion (blob_deleted_at + an audit event). A delete failure is logged
// and swallowed: the job is delivered regardless, and a leftover blob is a
// hygiene issue for a TTL/sweep to reclaim, not a correctness one.
func (h *Hub) deleteDeliveredBlob(ctx context.Context, jobID uuid.UUID, blobRef string) {
	if h.DeleteBlob == nil {
		log.Printf("printer-link: job %s delivered but blob deletion is not configured", jobID)
		return
	}
	if err := h.DeleteBlob(ctx, blobRef); err != nil {
		log.Printf("printer-link: job %s: delete blob %s: %v", jobID, blobRef, err)
		return
	}
	if err := h.Queries.SetJobBlobDeleted(ctx, jobID); err != nil {
		log.Printf("printer-link: job %s: mark blob_deleted_at: %v", jobID, err)
	}
	if err := h.Queries.InsertAuditEvent(ctx, db.InsertAuditEventParams{JobID: jobID, Action: "blob_deleted"}); err != nil {
		log.Printf("printer-link: job %s: audit blob_deleted: %v", jobID, err)
	}
	log.Printf("printer-link: job %s: ciphertext blob deleted", jobID)
}

// pumpDispatch relays Redis-published dispatch payloads down this
// printer's socket. It is the cross-node fan-in half of the dispatch
// model: any node's tryDispatch (Phase 4) PUBLISHes here; only the node
// that holds the actual socket -- this one -- can write to it.
func (h *Hub) pumpDispatch(ctx context.Context, conn *websocket.Conn, sub *redis.PubSub) {
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if err := conn.Write(ctx, websocket.MessageText, []byte(msg.Payload)); err != nil {
				log.Printf("printer-link: write dispatch frame: %v", err)
				return
			}
		}
	}
}
