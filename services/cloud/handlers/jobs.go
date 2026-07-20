package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"automail/cloud/authctx"
	"automail/cloud/db"
	"automail/cloud/dispatch"
	"automail/cloud/minioclient"

	"github.com/google/uuid"
)

type uploadURLRequest struct {
	RecipientID string `json:"recipient_id"`
	Filename    string `json:"filename"`
}

type uploadURLResponse struct {
	UploadURL string `json:"upload_url"`
	BlobRef   string `json:"blob_ref"`
	ExpiresIn int    `json:"expires_in"`
}

// UploadURL handles POST /jobs/upload-url. Auth optional. Generates a
// pre-signed MinIO PUT URL; the browser uploads the ciphertext directly --
// this server never receives the blob (plans/09-api-contracts.md).
func (s *Server) UploadURL(w http.ResponseWriter, r *http.Request) {
	var req uploadURLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid request body", "INVALID_BODY")
		return
	}

	const ttl = 15 * time.Minute
	// The browser PUTs directly to object storage, so the signed URL must use a
	// host the browser can reach (see Server.UploadPresigner). Server-side blob
	// ops keep using s.Minio (the internal endpoint).
	uploadURL, blobRef, err := minioclient.PresignedUploadURL(r.Context(), s.uploadPresigner(), ttl)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not generate upload URL", "INTERNAL")
		return
	}

	WriteJSON(w, http.StatusOK, uploadURLResponse{
		UploadURL: uploadURL,
		BlobRef:   blobRef,
		ExpiresIn: int(ttl.Seconds()),
	})
}

type createJobRequest struct {
	EncryptedKey string `json:"encrypted_key"` // base64 RSA-OAEP ciphertext
	BlobRef      string `json:"blob_ref"`
	RecipientID  string `json:"recipient_id"`
	PageCount    int32  `json:"page_count"`
}

type createJobResponse struct {
	JobID      string `json:"job_id"`
	Status     string `json:"status"`
	GuestToken string `json:"guest_token,omitempty"`
}

// hashGuestToken is the single definition of how a raw guest token maps
// to the value stored in jobs.guest_token_hash. Shared by token creation
// (newGuestToken, below) and verification (StreamJob's ?token= check) so
// the two sides can never drift.
func hashGuestToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// newGuestToken returns a random URL-safe token and its SHA-256 hash.
// Only the hash is ever stored (plans/02-security.md "Refresh token
// stored as a hash" applies the same pattern here) -- a stolen DB dump
// yields no usable token.
func newGuestToken() (raw string, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, hashGuestToken(raw), nil
}

// CreateJob handles POST /jobs. Auth optional: a Bearer token sets
// sender_id; otherwise the job is a guest submission with a one-time
// guest_token (plans/09-api-contracts.md). Every job is inserted as
// 'submitted', then dispatch.TryDispatch attempts immediate dispatch --
// the response status is "dispatching" or "queued" depending on the
// outcome (plans/05-cloud-server.md "Dispatch Logic").
//
// Zero-knowledge invariant: encrypted_key is decoded from base64 and
// passed straight to Postgres as opaque bytes. It is never decrypted,
// logged, or sent anywhere but the DB column here (plans/02-security.md).
func (s *Server) CreateJob(w http.ResponseWriter, r *http.Request) {
	var req createJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid request body", "INVALID_BODY")
		return
	}

	recipientID, err := uuid.Parse(req.RecipientID)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "recipient not found", "RECIPIENT_NOT_FOUND")
		return
	}

	resolved, err := s.Queries.ResolveRecipient(r.Context(), recipientID)
	if errors.Is(err, sql.ErrNoRows) {
		WriteError(w, http.StatusBadRequest, "recipient not found or slot unassigned", "RECIPIENT_NOT_FOUND")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "lookup failed", "INTERNAL")
		return
	}

	exists, err := minioclient.BlobExists(r.Context(), s.Minio, req.BlobRef)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "blob check failed", "INTERNAL")
		return
	}
	if !exists {
		WriteError(w, http.StatusUnprocessableEntity, "blob_ref not found", "INVALID_BLOB_REF")
		return
	}

	encryptedKey, err := base64.StdEncoding.DecodeString(req.EncryptedKey)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "encrypted_key must be base64", "INVALID_BODY")
		return
	}

	params := db.InsertJobParams{
		MailboxID:    resolved.MailboxID,
		SlotID:       resolved.SlotID,
		EncryptedKey: encryptedKey,
		BlobRef:      req.BlobRef,
		PageCount:    req.PageCount,
	}

	var guestToken string
	if senderID, ok := authctx.SenderID(r.Context()); ok {
		params.SenderID = uuid.NullUUID{UUID: senderID, Valid: true}
	} else {
		raw, hash, err := newGuestToken()
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "could not generate guest token", "INTERNAL")
			return
		}
		guestToken = raw
		params.GuestTokenHash = sql.NullString{String: hash, Valid: true}
	}

	job, err := s.Queries.InsertJob(r.Context(), params)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not create job", "INTERNAL")
		return
	}

	var actorID uuid.NullUUID
	if senderID, ok := authctx.SenderID(r.Context()); ok {
		actorID = uuid.NullUUID{UUID: senderID, Valid: true}
	}
	if err := s.Queries.InsertAuditEvent(r.Context(), db.InsertAuditEventParams{
		JobID:   job.ID,
		Action:  "job_submitted",
		ActorID: actorID,
	}); err != nil {
		WriteError(w, http.StatusInternalServerError, "could not write audit event", "INTERNAL")
		return
	}

	// Attempt immediate dispatch (plans/05-cloud-server.md "Immediate
	// Dispatch"). A dispatch-layer error (Redis/Postgres unreachable, not
	// a normal "blocked" outcome -- that returns status "queued", not an
	// error) surfaces as a 500: the job row exists as 'submitted', so it
	// isn't lost, but nothing has queued it onto jobs:pending yet either,
	// so the sender needs to know the submission didn't fully land rather
	// than silently trusting a queued state that was never actually set up.
	ref := dispatch.FromJob(job.ID, resolved.MailboxID, resolved.SlotID, encryptedKey, req.BlobRef)
	status, err := dispatch.TryDispatch(r.Context(), s.Dispatcher, ref)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not dispatch job", "INTERNAL")
		return
	}

	WriteJSON(w, http.StatusAccepted, createJobResponse{
		JobID:      job.ID.String(),
		Status:     status,
		GuestToken: guestToken,
	})
}

// streamEvent is the SSE `data:` payload for GET /jobs/:id/stream
// (plans/09-api-contracts.md). It is the internal job:<id>:status
// pub/sub payload (link.statusPayload: status + optional error) with
// job_id added back in -- the Redis channel name scopes the job
// internally, so the hub deliberately omits it, and this handler is
// responsible for restoring it to match the documented wire format:
//
//	data: {"job_id":"uuid","status":"printing"}\n\n
type streamEvent struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"` // set by the printer only when Status == "failed"
}

// isTerminalStatus reports whether a job status ends the stream:
// "Connection closes when status reaches delivered or failed"
// (plans/09-api-contracts.md).
func isTerminalStatus(status string) bool {
	return status == "delivered" || status == "failed"
}

// StreamJob handles GET /jobs/:id/stream: the Server-Sent Events relay
// that pushes job status transitions to the sender's browser without
// polling (plans/05-cloud-server.md, plans/10-implementation-roadmap.md
// Phase 5). The printer-link hub already updates Postgres and PUBLISHes
// each transition to job:<id>:status (link.Hub.onStatus); this handler is
// the fan-out half: it subscribes to that channel and relays events to
// whichever browser connection this node happens to hold. Because every
// node publishes to and subscribes through the same Redis, a status frame
// arriving on the node that owns the printer's socket still reaches an
// SSE client connected to any other node -- the mirror image of dispatch
// fan-in (docs/study/17-sse-vs-websocket-redis-fanout.md).
//
// Auth (plans/09-api-contracts.md): a Bearer JWT whose sender owns the
// job, OR ?token=<guest_token> matching jobs.guest_token_hash. The
// token rides in the query string because the browser EventSource API
// cannot set request headers.
//
// The stream carries job_id + status (+ printer error text on failure)
// and nothing else -- never encrypted_key, blob_ref, or routing metadata
// (zero-knowledge invariant, plans/02-security.md).
func (s *Server) StreamJob(w http.ResponseWriter, r *http.Request) {
	jobID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		WriteError(w, http.StatusNotFound, "job not found", "NOT_FOUND")
		return
	}

	job, err := s.Queries.GetJobForStream(r.Context(), jobID)
	if errors.Is(err, sql.ErrNoRows) {
		WriteError(w, http.StatusNotFound, "job not found", "NOT_FOUND")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "lookup failed", "INTERNAL")
		return
	}

	if !authorizeStream(w, r, job) {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, http.StatusInternalServerError, "streaming unsupported", "INTERNAL")
		return
	}

	// Subscribe BEFORE reading the status snapshot below, and wait for
	// the server's subscribe ack (same discipline as link.Hub.Accept's
	// dispatch subscription): a transition that lands in the gap between
	// "read status" and "subscription active" would otherwise be lost --
	// not in the snapshot, never delivered by the channel -- leaving the
	// client stuck on a stale status forever. Subscribing first makes the
	// two sources overlap instead of gap: the worst case is seeing the
	// same transition twice (once from the snapshot, once from pub/sub),
	// and duplicate status events are harmless to render.
	sub := s.Redis.Subscribe(r.Context(), "job:"+jobID.String()+":status")
	defer sub.Close()
	if _, err := sub.Receive(r.Context()); err != nil {
		WriteError(w, http.StatusInternalServerError, "could not subscribe to status channel", "INTERNAL")
		return
	}

	// Re-read the status now that the subscription is live -- the row
	// fetched above for the auth check may already be stale. This becomes
	// the initial snapshot event: the plans don't mandate one, but without
	// it a client connecting after a transition (or after delivery) would
	// hang waiting for an event that already fired. With it, connecting to
	// an already-delivered job yields one event and a clean close.
	job, err = s.Queries.GetJobForStream(r.Context(), jobID)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "lookup failed", "INTERNAL")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	if !writeSSE(w, flusher, streamEvent{JobID: jobID.String(), Status: job.Status}) {
		return
	}
	if isTerminalStatus(job.Status) {
		return
	}

	ch := sub.Channel()
	for {
		select {
		case <-r.Context().Done():
			// Client went away (or server shutdown) -- the deferred
			// sub.Close() unsubscribes this node from the channel.
			return
		case msg, chOpen := <-ch:
			if !chOpen {
				return
			}
			var ev streamEvent
			if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil || ev.Status == "" {
				// Payload content is not logged -- only statusPayload ever
				// rides this channel, but log hygiene is cheap.
				log.Printf("stream: job %s: ignoring malformed status payload (err=%v)", jobID, err)
				continue
			}
			ev.JobID = jobID.String() // add job_id back in (see streamEvent doc)
			if !writeSSE(w, flusher, ev) {
				return
			}
			if isTerminalStatus(ev.Status) {
				return
			}
		}
	}
}

// authorizeStream applies the GET /jobs/:id/stream auth rule and writes
// the error response itself when the request is rejected. Grant if
// EITHER credential checks out:
//
//   - JWT ownership: optionalAuth already validated the Bearer token and
//     stashed the sender in the request context; the sender must own the
//     job. A valid JWT for the WRONG sender gets 404, not 403 -- job IDs
//     are unguessable UUIDs, and 404 avoids confirming to another
//     account that the job exists at all.
//   - Guest token: SHA-256 of ?token= must match jobs.guest_token_hash
//     (the same hash newGuestToken stored at submission). Compared
//     constant-time as hygiene, though hash comparison isn't practically
//     timing-attackable. A wrong token is 403 GUEST_TOKEN_INVALID.
//
// No credential at all is 401 UNAUTHORIZED.
func authorizeStream(w http.ResponseWriter, r *http.Request, job db.GetJobForStreamRow) bool {
	senderID, hasSender := authctx.SenderID(r.Context())
	if hasSender && job.SenderID.Valid && job.SenderID.UUID == senderID {
		return true
	}

	token := r.URL.Query().Get("token")
	if token != "" && job.GuestTokenHash.Valid &&
		subtle.ConstantTimeCompare([]byte(hashGuestToken(token)), []byte(job.GuestTokenHash.String)) == 1 {
		return true
	}

	switch {
	case token != "":
		WriteError(w, http.StatusForbidden, "invalid guest token", "GUEST_TOKEN_INVALID")
	case hasSender:
		WriteError(w, http.StatusNotFound, "job not found", "NOT_FOUND")
	default:
		WriteError(w, http.StatusUnauthorized, "authentication required", "UNAUTHORIZED")
	}
	return false
}

// writeSSE writes one `data: <json>\n\n` frame and flushes it
// immediately -- SSE is only "real time" if nothing between the handler
// and the socket buffers the event. A write error means the client is
// gone; the caller stops streaming.
func writeSSE(w http.ResponseWriter, flusher http.Flusher, ev streamEvent) bool {
	buf, err := json.Marshal(ev)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", buf); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

type historyJob struct {
	JobID       string  `json:"job_id"`
	Status      string  `json:"status"`
	PageCount   int32   `json:"page_count"`
	CreatedAt   string  `json:"created_at"`
	DeliveredAt *string `json:"delivered_at,omitempty"`
}

// ListMyJobs handles GET /jobs (authenticated). Returns the caller's own jobs,
// newest first -- the account-flow counterpart to the guest's saved
// guest_token: a logged-in sender sees all their jobs without holding any
// token. Metadata only: never encrypted_key or blob_ref (zero-knowledge,
// plans/02-security.md; the query itself does not select them). requireAuth
// gates the route, so a missing sender here is a defensive 401.
func (s *Server) ListMyJobs(w http.ResponseWriter, r *http.Request) {
	senderID, ok := authctx.SenderID(r.Context())
	if !ok {
		WriteError(w, http.StatusUnauthorized, "authentication required", "UNAUTHORIZED")
		return
	}

	rows, err := s.Queries.GetJobsBySender(r.Context(), uuid.NullUUID{UUID: senderID, Valid: true})
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not list jobs", "INTERNAL")
		return
	}

	jobs := make([]historyJob, 0, len(rows))
	for _, row := range rows {
		hj := historyJob{
			JobID:     row.ID.String(),
			Status:    row.Status,
			PageCount: row.PageCount,
			CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
		}
		if row.DeliveredAt.Valid {
			delivered := row.DeliveredAt.Time.UTC().Format(time.RFC3339)
			hj.DeliveredAt = &delivered
		}
		jobs = append(jobs, hj)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}
