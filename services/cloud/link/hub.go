package link

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"

	"automail/cloud/db"
	"automail/cloud/store"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// Hub owns the printer-link upgrade handler and the read loop for every
// connected printer on this node. It is the cloud-server-side half of
// plans/04-printer-microservice.md's "Connection lifecycle".
type Hub struct {
	Registry *Registry
	Redis    *redis.Client
	Queries  *db.Queries
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

	if err := store.SetPrinterState(ctx, h.Redis, reg.MailboxID, registerToState(reg)); err != nil {
		log.Printf("printer-link: mailbox %s: seed state cache: %v", reg.MailboxID, err)
	}

	// The owner node (this one) subscribes so any node's tryDispatch can
	// route a job to this socket via PUBLISH -- see "Why publish instead
	// of call" in plans/05-cloud-server.md. pumpDispatch exits on its own
	// once ctx is cancelled (read loop below returns -> caller cancels).
	pumpCtx, cancelPump := context.WithCancel(ctx)
	defer cancelPump()
	sub := h.Redis.Subscribe(pumpCtx, "mailbox:"+reg.MailboxID+":dispatch")
	defer sub.Close()
	go h.pumpDispatch(pumpCtx, conn, sub)

	return h.readLoop(ctx, conn, reg.MailboxID)
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
	if frame.Status == "idle" {
		// Wakes the Phase 4 dispatcher goroutine, which re-evaluates the
		// blocked-jobs stream whenever a printer becomes available. No
		// dispatcher subscribes yet in Phase 3 -- PUBLISH with zero
		// subscribers is a harmless no-op.
		h.Redis.Publish(ctx, "mailbox:"+mailboxID+":available", "1")
	}
}

// onStatus applies a job lifecycle update to Postgres and relays it to
// any sender watching via SSE (Phase 5). "delivered" additionally
// triggers blob cleanup in later phases; Phase 3's dev-mode dispatch
// already deletes its dummy file itself, so no MinIO call happens here
// yet -- that lands with Phase 4/6's real blob lifecycle.
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
	h.Redis.Publish(ctx, "job:"+jobID.String()+":status", payload)

	log.Printf("printer-link: job %s -> %s (mailbox %s)", job.ID, job.Status, job.MailboxID)
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
