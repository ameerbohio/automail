package dispatch

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"automail/cloud/store"
)

// TestEligible exercises plans/03-scaling.md's "Dispatch Eligibility
// Check" against the same Redis-cached printer state shape link.Hub
// writes on register/state frames. No Postgres needed -- eligible() only
// reads mailbox:<id>:state.
func TestEligible(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	ctx := context.Background()

	const mailboxID = "mailbox-1"
	const slotID = "slot-1"

	// No state cached yet -- store.GetPrinterState stubs to idle with no
	// slot entries, so an unknown slot is "unknown capacity", not "free".
	if eligible(ctx, rdb, mailboxID, slotID) {
		t.Fatal("expected ineligible: no cached state means unknown slot capacity")
	}

	// Printer idle but slot full.
	mustSetState(t, ctx, rdb, mailboxID, store.PrinterState{
		Status:        "idle",
		SlotOccupancy: map[string]store.SlotState{slotID: {Current: 5, Max: 5}},
	})
	if eligible(ctx, rdb, mailboxID, slotID) {
		t.Fatal("expected ineligible: slot at max capacity")
	}

	// Printer busy, slot has room.
	mustSetState(t, ctx, rdb, mailboxID, store.PrinterState{
		Status:        "printing",
		SlotOccupancy: map[string]store.SlotState{slotID: {Current: 0, Max: 5}},
	})
	if eligible(ctx, rdb, mailboxID, slotID) {
		t.Fatal("expected ineligible: printer not idle")
	}

	// Printer idle, slot has room -- the only eligible combination.
	mustSetState(t, ctx, rdb, mailboxID, store.PrinterState{
		Status:        "idle",
		SlotOccupancy: map[string]store.SlotState{slotID: {Current: 2, Max: 5}},
	})
	if !eligible(ctx, rdb, mailboxID, slotID) {
		t.Fatal("expected eligible: printer idle, slot has room")
	}
}

func mustSetState(t *testing.T, ctx context.Context, rdb *redis.Client, mailboxID string, state store.PrinterState) {
	t.Helper()
	if err := store.SetPrinterState(ctx, rdb, mailboxID, state); err != nil {
		t.Fatalf("SetPrinterState: %v", err)
	}
}

// TestJobRefRoundTrip proves a JobRef survives the XADD encode / XREADGROUP
// decode round trip -- toXAddValues and JobRefFromValues must agree on
// every field, since a blocked job's only record while queued is what's
// sitting in the jobs:pending stream.
func TestJobRefRoundTrip(t *testing.T) {
	want := JobRef{
		JobID:        uuid.New().String(),
		MailboxID:    uuid.New().String(),
		SlotID:       uuid.New().String(),
		EncryptedKey: "YWJjMTIz",
		BlobRef:      "blobs/" + uuid.New().String(),
	}

	values := want.toXAddValues()
	// Simulate what go-redis hands back from XREADGROUP: everything comes
	// back as interface{} wrapping a string.
	asInterfaceMap := make(map[string]interface{}, len(values))
	for k, v := range values {
		asInterfaceMap[k] = v
	}

	got, err := JobRefFromValues(asInterfaceMap)
	if err != nil {
		t.Fatalf("JobRefFromValues: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestJobRefFromValues_MissingField confirms a malformed stream message
// (missing a required field) is reported as an error rather than
// silently producing a zero-valued JobRef that would dispatch garbage.
func TestJobRefFromValues_MissingField(t *testing.T) {
	values := map[string]interface{}{
		"job_id":     "abc",
		"mailbox_id": "def",
		// slot_id, encrypted_key, blob_ref deliberately omitted
	}
	if _, err := JobRefFromValues(values); err == nil {
		t.Fatal("expected an error for a stream message missing required fields")
	}
}

// TestTryDispatch_NotEligibleEnqueues proves the not-eligible path is a
// pure Redis operation that never touches Postgres: when the printer
// isn't idle, TryDispatch must XADD onto jobs:pending and return "queued"
// without calling claimJob (Deps.SQLDB is left nil here -- a claimJob
// call would nil-panic, so reaching the assertions below is itself proof
// the claim path was skipped).
func TestTryDispatch_NotEligibleEnqueues(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	ctx := context.Background()

	ref := JobRef{
		JobID:        uuid.New().String(),
		MailboxID:    "mailbox-1",
		SlotID:       "slot-1",
		EncryptedKey: "YWJjMTIz",
		BlobRef:      "blobs/x",
	}

	// No cached state -- printer state is "unknown", which eligible()
	// treats as not dispatchable.
	deps := Deps{Redis: rdb} // SQLDB, Queries, Minio intentionally nil/unused
	status, err := TryDispatch(ctx, deps, ref)
	if err != nil {
		t.Fatalf("TryDispatch: %v", err)
	}
	if status != "queued" {
		t.Fatalf("got status %q, want %q", status, "queued")
	}

	// No consumer group exists in this test (EnsureGroup wasn't called),
	// so read the raw stream entries directly via XRANGE instead of
	// XREADGROUP, which requires a group.
	raw, err := rdb.XRange(ctx, PendingStream, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRange: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("got %d stream entries, want 1", len(raw))
	}
	if raw[0].Values["job_id"] != ref.JobID {
		t.Fatalf("got job_id %v, want %v", raw[0].Values["job_id"], ref.JobID)
	}
}
