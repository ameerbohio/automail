package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	uploadURL, blobRef, err := minioclient.PresignedUploadURL(r.Context(), s.Minio, ttl)
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
	sum := sha256.Sum256([]byte(raw))
	hash = base64.RawURLEncoding.EncodeToString(sum[:])
	return raw, hash, nil
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
