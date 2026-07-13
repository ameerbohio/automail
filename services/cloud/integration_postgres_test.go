//go:build integration

package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
)

// TestIntegration_SchemaAppliesAndPgcrypto proves schema.sql applied clean
// on container init AND that the pgcrypto extension actually works -- not
// just that `CREATE EXTENSION` ran, but that pgp_sym_encrypt/decrypt round
// trips, since every PII column (senders.email_enc, residents.name_enc)
// depends on it. A fake can't prove the extension is present in the image.
func TestIntegration_SchemaAppliesAndPgcrypto(t *testing.T) {
	sqlDB, _ := startPostgres(t)
	ctx := context.Background()

	// Every table the schema declares should exist.
	for _, table := range []string{
		"buildings", "mailboxes", "senders", "refresh_tokens",
		"mailbox_slots", "residents", "jobs", "audit_events",
	} {
		var exists bool
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = $1)`,
			table,
		).Scan(&exists); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if !exists {
			t.Fatalf("expected table %s to exist after schema init", table)
		}
	}

	// pgcrypto actually round-trips.
	var out string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT pgp_sym_decrypt(pgp_sym_encrypt('hello@example.com', $1), $1)`,
		"test-app-key",
	).Scan(&out); err != nil {
		t.Fatalf("pgcrypto round trip: %v", err)
	}
	if out != "hello@example.com" {
		t.Fatalf("pgcrypto round trip = %q, want %q", out, "hello@example.com")
	}
}

// TestIntegration_AuditTriggerBlocksMutation makes the roadmap's prose
// promise ("audit_events rows are immutable") an executable guard: the
// BEFORE UPDATE OR DELETE trigger must reject both, so the audit trail
// can't be rewritten or erased. INSERT must still work -- immutability is
// append-only, not read-only.
func TestIntegration_AuditTriggerBlocksMutation(t *testing.T) {
	sqlDB, _ := startPostgres(t)
	ctx := context.Background()
	auditID := seedAuditEvent(t, sqlDB)

	const wantMsg = "audit_events rows are immutable"

	_, err := sqlDB.ExecContext(ctx, `DELETE FROM audit_events WHERE id = $1`, auditID)
	if err == nil {
		t.Fatal("DELETE FROM audit_events succeeded; trigger did not block it")
	}
	if !strings.Contains(err.Error(), wantMsg) {
		t.Fatalf("DELETE error = %v, want it to contain %q", err, wantMsg)
	}

	_, err = sqlDB.ExecContext(ctx, `UPDATE audit_events SET action = 'tampered' WHERE id = $1`, auditID)
	if err == nil {
		t.Fatal("UPDATE audit_events succeeded; trigger did not block it")
	}
	if !strings.Contains(err.Error(), wantMsg) {
		t.Fatalf("UPDATE error = %v, want it to contain %q", err, wantMsg)
	}

	// The row is still there and unchanged.
	var action string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT action FROM audit_events WHERE id = $1`, auditID,
	).Scan(&action); err != nil {
		t.Fatalf("read back audit row: %v", err)
	}
	if action != "job_submitted" {
		t.Fatalf("audit action = %q, want it unchanged as %q", action, "job_submitted")
	}

	// Appending a new event still works (append-only, not frozen).
	var jobID string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT job_id FROM audit_events WHERE id = $1`, auditID,
	).Scan(&jobID); err != nil {
		t.Fatalf("read job_id: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO audit_events (job_id, action) VALUES ($1, 'job_delivered')`, jobID,
	); err != nil {
		t.Fatalf("INSERT new audit event should succeed: %v", err)
	}
}

// TestIntegration_SelectForUpdateNowaitContention proves the
// double-dispatch guard's core: with one transaction holding the row lock,
// a second node's LockJobForDispatch (FOR UPDATE NOWAIT) returns the 55P03
// lock_not_available error *immediately* -- it does not block waiting for
// the lock. A hang here would mean two nodes serialize on every contended
// job instead of one skipping; the whole point of NOWAIT is that the loser
// falls through fast. This is the behavior a fake DB cannot verify.
func TestIntegration_SelectForUpdateNowaitContention(t *testing.T) {
	sqlDB, q := startPostgres(t)
	ctx := context.Background()
	jobID := seedJob(t, sqlDB)

	// Tx A claims the row lock and holds it.
	txA, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin txA: %v", err)
	}
	defer txA.Rollback() //nolint:errcheck
	if _, err := q.WithTx(txA).LockJobForDispatch(ctx, jobID); err != nil {
		t.Fatalf("txA LockJobForDispatch (should acquire): %v", err)
	}

	// Tx B races for the same row. It must get 55P03 fast, not hang.
	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		txB, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer txB.Rollback() //nolint:errcheck
		_, lockErr := q.WithTx(txB).LockJobForDispatch(ctx, jobID)
		done <- result{err: lockErr}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("txB acquired the lock while txA held it; NOWAIT did not error")
		}
		var pqErr *pq.Error
		if !errors.As(r.err, &pqErr) || string(pqErr.Code) != "55P03" {
			t.Fatalf("txB error = %v, want Postgres 55P03 lock_not_available", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("txB blocked >5s on a NOWAIT lock -- it should have failed immediately")
	}

	// Release txA; the row is now claimable again (no lingering lock).
	_ = txA.Rollback()
	if _, err := q.LockJobForDispatch(ctx, jobID); err != nil {
		t.Fatalf("after txA rollback the row should be lockable again: %v", err)
	}
}

// TestIntegration_LockJobForDispatchOnlyClaimable confirms the query's
// status filter against real Postgres: a terminal ('delivered') job is not
// returned (sql.ErrNoRows), so claimJob maps it to "already settled"
// rather than re-dispatching a finished job.
func TestIntegration_LockJobForDispatchOnlyClaimable(t *testing.T) {
	sqlDB, q := startPostgres(t)
	ctx := context.Background()
	jobID := seedJob(t, sqlDB)

	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE jobs SET status = 'delivered', delivered_at = now() WHERE id = $1`, jobID,
	); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}

	_, err := q.LockJobForDispatch(ctx, jobID)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("LockJobForDispatch on a delivered job = %v, want sql.ErrNoRows", err)
	}
}
