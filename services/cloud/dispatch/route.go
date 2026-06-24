// Package dispatch implements Phase 4's job dispatch: the immediate-path
// attempt made right after a job is created, the Redis Stream fallback
// queue for jobs that can't go out immediately, and the consumer-group
// dispatcher goroutine that drains that queue as printers become
// available. See plans/03-scaling.md "Job Queue: Redis Streams +
// Consumer Groups" and plans/05-cloud-server.md "Dispatch Logic".
package dispatch

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"time"

	"automail/cloud/db"
	"automail/cloud/minioclient"
	"automail/cloud/store"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/minio/minio-go/v7"
	"github.com/redis/go-redis/v9"
)

// PendingStream and Group are the Redis Stream + consumer group names from
// plans/03-scaling.md. One stream serves every mailbox -- the consumer
// group is what guarantees a queued job is picked up by exactly one node,
// not which mailbox it belongs to.
const (
	PendingStream = "jobs:pending"
	ConsumerGroup = "dispatchers"

	// presignTTL is how long the pre-signed MinIO GET URL handed to the
	// printer in a dispatch frame stays valid. Short-lived: the printer is
	// expected to fetch the blob within the dispatch window, not hold the
	// URL for later.
	presignTTL = 5 * time.Minute
)

// lockNotAvailable is Postgres error code 55P03, returned by
// `FOR UPDATE NOWAIT` when another session already holds the row lock.
// This is the expected, non-exceptional outcome of two nodes racing for
// the same job -- the loser just skips it (plans/03-scaling.md "If the
// FOR UPDATE NOWAIT fails... the current node skips this job").
const lockNotAvailable = "55P03"

// Deps bundles everything tryDispatch needs. A plain struct (not the
// handlers.Server type) so this package doesn't import handlers -- it's
// the other way around, handlers/jobs.go imports dispatch.
type Deps struct {
	SQLDB   *sql.DB
	Queries *db.Queries
	Redis   *redis.Client
	Minio   *minio.Client
}

// dispatchPayload is the JSON published to mailbox:<id>:dispatch and
// written down the printer's socket by link.Hub.pumpDispatch. Field names
// match link.Frame's dispatch-relevant fields so the printer-link read
// loop on the printer side (plans/04-printer-microservice.md) can decode
// it directly.
type dispatchPayload struct {
	Type         string `json:"type"`
	JobID        string `json:"job_id"`
	EncryptedKey string `json:"encrypted_key"`
	BlobURL      string `json:"blob_url"`
}

// JobRef is what XADD stores for a blocked job -- enough to retry
// dispatch later without going back to Postgres first. slot_id rides
// along even though plans/03-scaling.md's XAdd example omits it: the
// eligibility check needs it to look up SlotOccupancy[job.SlotID], and a
// queued job's slot doesn't change between enqueue and retry.
type JobRef struct {
	JobID        string
	MailboxID    string
	SlotID       string
	EncryptedKey string // base64
	BlobRef      string
}

func (f JobRef) toXAddValues() map[string]any {
	return map[string]any{
		"job_id":        f.JobID,
		"mailbox_id":    f.MailboxID,
		"slot_id":       f.SlotID,
		"encrypted_key": f.EncryptedKey,
		"blob_ref":      f.BlobRef,
	}
}

func JobRefFromValues(values map[string]interface{}) (JobRef, error) {
	get := func(k string) (string, error) {
		v, ok := values[k]
		if !ok {
			return "", errors.New("dispatch: stream message missing field " + k)
		}
		s, ok := v.(string)
		if !ok {
			return "", errors.New("dispatch: stream field " + k + " is not a string")
		}
		return s, nil
	}
	var f JobRef
	var err error
	if f.JobID, err = get("job_id"); err != nil {
		return f, err
	}
	if f.MailboxID, err = get("mailbox_id"); err != nil {
		return f, err
	}
	if f.SlotID, err = get("slot_id"); err != nil {
		return f, err
	}
	if f.EncryptedKey, err = get("encrypted_key"); err != nil {
		return f, err
	}
	if f.BlobRef, err = get("blob_ref"); err != nil {
		return f, err
	}
	return f, nil
}

// blocked is attemptDispatch's sentinel for "could not go out right now"
// -- printer not idle, slot full, contended claim, or no live socket. It
// is deliberately not an error: a job that can't dispatch immediately is
// a normal, expected outcome, not a failure.
const blocked = "blocked"

// settled is attemptDispatch's sentinel for "this job is no longer this
// node's concern, and never will be again" -- the row was already
// terminal (delivered/failed) or already claimed and moved past
// 'submitted'/'queued' by the time this node looked, i.e. NOT the
// transient "another node is mid-claim right now, try again later" case
// that `blocked` covers. Distinguishing the two matters because the
// caller's response differs: a stream message for a settled job should
// be ACK'd and dropped (nothing to retry), never re-XADD'd or left
// pending to be retried forever.
const settled = "settled"

// TryDispatch is the entry point for a freshly submitted job
// (handlers/jobs.go's CreateJob, right after InsertJob). Returns the
// job's resulting status ("dispatching" or "queued") for the POST /jobs
// response (plans/09-api-contracts.md). If the immediate attempt is
// blocked, this is the job's *first* trip through dispatch -- nothing is
// sitting in jobs:pending for it yet -- so this path XADDs it. A
// `settled` outcome can't realistically happen for a job this fresh (its
// row is still 'submitted'), but is handled defensively by reporting
// "dispatching" without enqueueing -- something else already moved the
// row past a claimable status, so there is nothing left for this node to
// queue.
func TryDispatch(ctx context.Context, d Deps, f JobRef) (status string, err error) {
	outcome, err := attemptDispatch(ctx, d, f)
	if err != nil {
		return "", err
	}
	switch outcome {
	case blocked:
		return enqueue(ctx, d.Redis, f)
	case settled:
		return "dispatching", nil
	default:
		return outcome, nil
	}
}

// Retry re-attempts dispatch for a job already sitting in jobs:pending
// (the dispatcher goroutine's path -- dispatcher.go's drain/reclaim).
// Unlike TryDispatch, a still-blocked outcome here must NOT XADD a fresh
// stream entry: the job's existing entry is already in this consumer's
// PEL, and self-re-enqueueing on every retry would make an offline
// printer's backlog grow without bound (every mailbox:<id>:available
// event -- or XAUTOCLAIM sweep -- would multiply the entry count). The
// caller (dispatcher.go) is responsible for leaving a still-blocked
// message un-ACK'd so XAUTOCLAIM naturally retries it later instead.
//
// done reports whether the stream message should be ACK'd: true for
// both "actually dispatched" and "settled" (already terminal/claimed --
// nothing to retry, drop it), false only for blocked (transient,
// genuinely worth retrying).
func Retry(ctx context.Context, d Deps, f JobRef) (done bool, err error) {
	outcome, err := attemptDispatch(ctx, d, f)
	if err != nil {
		return false, err
	}
	return outcome != blocked, nil
}

// attemptDispatch is the shared core of TryDispatch and Retry: the
// eligibility check, the Postgres double-dispatch guard, and the
// publish-to-the-owner-node fan-in. It never touches the Redis Stream --
// callers decide what "blocked" means for their caller (XADD vs. leave
// pending).
func attemptDispatch(ctx context.Context, d Deps, f JobRef) (status string, err error) {
	if !eligible(ctx, d.Redis, f.MailboxID, f.SlotID) {
		return blocked, nil
	}

	outcome, err := claimJob(ctx, d.SQLDB, d.Queries, f.JobID)
	if err != nil {
		return "", err
	}
	switch outcome {
	case claimLost:
		// Another node holds the row lock for this exact job right now
		// (NOWAIT contention) -- genuinely transient, worth retrying.
		return blocked, nil
	case claimSettled:
		// The row is no longer 'submitted'/'queued' -- already terminal,
		// or already moved past this status by a claim that already
		// committed. Nothing for this node to do, and nothing to retry --
		// logged since this is the path that silently drops a stale
		// stream entry (Retry's caller ACKs and moves on with no audit
		// event of its own).
		log.Printf("dispatch: job %s: already settled (not submitted/queued), dropping", f.JobID)
		return settled, nil
	}

	readURL, err := minioclient.PresignedReadURL(ctx, d.Minio, f.BlobRef, presignTTL)
	if err != nil {
		// Best-effort revert so the job isn't stuck in 'dispatching' with
		// nobody driving it forward.
		_ = revert(ctx, d.Queries, f.JobID)
		return "", err
	}

	payload, err := json.Marshal(dispatchPayload{
		Type:         "dispatch",
		JobID:        f.JobID,
		EncryptedKey: f.EncryptedKey,
		BlobURL:      readURL,
	})
	if err != nil {
		_ = revert(ctx, d.Queries, f.JobID)
		return "", err
	}

	receivers, err := d.Redis.Publish(ctx, "mailbox:"+f.MailboxID+":dispatch", payload).Result()
	if err != nil {
		_ = revert(ctx, d.Queries, f.JobID)
		return "", err
	}
	if receivers == 0 {
		// No node holds a live socket for this printer -- it's offline.
		// Revert the claim; let the caller decide how to re-queue
		// (plans/05-cloud-server.md "Presence and liveness").
		if err := revert(ctx, d.Queries, f.JobID); err != nil {
			return "", err
		}
		return blocked, nil
	}

	jobID, err := uuid.Parse(f.JobID)
	if err != nil {
		return "", err
	}
	if err := d.Queries.InsertAuditEvent(ctx, db.InsertAuditEventParams{JobID: jobID, Action: "job_dispatched"}); err != nil {
		log.Printf("dispatch: job %s: write audit event: %v", f.JobID, err)
	}
	return "dispatching", nil
}

// FromJob builds JobRef out of a freshly inserted job row. Exported
// for handlers/jobs.go, which has all these fields in hand right after
// InsertJob without needing a round trip back to Postgres.
func FromJob(jobID uuid.UUID, mailboxID uuid.UUID, slotID uuid.UUID, encryptedKey []byte, blobRef string) JobRef {
	return JobRef{
		JobID:        jobID.String(),
		MailboxID:    mailboxID.String(),
		SlotID:       slotID.String(),
		EncryptedKey: base64.StdEncoding.EncodeToString(encryptedKey),
		BlobRef:      blobRef,
	}
}

// eligible runs plans/03-scaling.md's "Dispatch Eligibility Check"
// against the Redis-cached printer state: the printer must be idle and
// the job's slot must have room.
func eligible(ctx context.Context, rdb *redis.Client, mailboxID, slotID string) bool {
	state, err := store.GetPrinterState(ctx, rdb, mailboxID)
	if err != nil {
		log.Printf("dispatch: get printer state for mailbox %s: %v", mailboxID, err)
		return false
	}
	if state.Status != "idle" {
		return false
	}
	slot, ok := state.SlotOccupancy[slotID]
	if !ok {
		// Unknown capacity for this slot -- treat as not dispatchable
		// rather than assuming room, per store.GetPrinterState's doc
		// comment on Phase 4's eligibility distinction.
		return false
	}
	return slot.Current < slot.Max
}

// claimOutcome is claimJob's result. claimed means this node committed
// the 'submitted'/'queued' -> 'dispatching' transition and owns the job
// from here. claimLost and claimSettled are both "not claimed" but are
// NOT interchangeable -- see their doc comments and attemptDispatch's
// switch on this value.
type claimOutcome int

const (
	// claimed: this node successfully transitioned the row to
	// 'dispatching' and should proceed to publish the dispatch frame.
	claimed claimOutcome = iota
	// claimLost: another node holds the NOWAIT row lock for this exact
	// job right now (55P03). Transient -- the row is still
	// 'submitted'/'queued' as far as this node knows, just contended.
	// Worth retrying.
	claimLost
	// claimSettled: the row is no longer in a claimable status --
	// already 'dispatching'/'printing' (another node's claim already
	// committed) or already terminal ('delivered'/'failed'). Permanent
	// from this node's perspective; nothing to retry.
	claimSettled
)

// claimJob is the SELECT FOR UPDATE NOWAIT double-dispatch guard
// (plans/03-scaling.md "Dispatch Eligibility Check", plans/08-data-models
// status transitions). Runs LockJobForDispatch + MarkJobDispatching in a
// single transaction so the lock is held across both statements.
func claimJob(ctx context.Context, sqlDB *sql.DB, q *db.Queries, jobIDStr string) (claimOutcome, error) {
	jobID, err := uuid.Parse(jobIDStr)
	if err != nil {
		return claimSettled, err
	}

	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return claimSettled, err
	}
	defer tx.Rollback() //nolint:errcheck -- no-op once committed

	txq := q.WithTx(tx)
	if _, err := txq.LockJobForDispatch(ctx, jobID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Not found in a claimable status: already claimed by a
			// committed transaction, or already terminal. Permanent.
			return claimSettled, nil
		}
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == lockNotAvailable {
			// Row is claimable but another node holds the lock on it
			// right now. Transient.
			return claimLost, nil
		}
		return claimSettled, err
	}
	if err := txq.MarkJobDispatching(ctx, jobID); err != nil {
		return claimSettled, err
	}
	if err := tx.Commit(); err != nil {
		return claimSettled, err
	}
	return claimed, nil
}

func revert(ctx context.Context, q *db.Queries, jobIDStr string) error {
	jobID, err := uuid.Parse(jobIDStr)
	if err != nil {
		return err
	}
	return q.RequeueJob(ctx, jobID)
}

// enqueue appends a blocked job to jobs:pending. Always returns status
// "queued" -- by the time this runs the job is either still 'submitted'
// (never claimed) or was reverted back to 'queued' (lost the publish
// race), so the queued Stream entry and the Postgres row agree.
func enqueue(ctx context.Context, rdb *redis.Client, f JobRef) (string, error) {
	_, err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: PendingStream,
		Values: f.toXAddValues(),
	}).Result()
	if err != nil {
		return "", err
	}
	return "queued", nil
}
