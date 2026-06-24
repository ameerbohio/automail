package link

import (
	"encoding/json"
	"testing"

	"automail/cloud/store"
)

func TestToStoreSlots(t *testing.T) {
	in := map[string]SlotState{"slot-1": {Current: 2, Max: 5}}
	out := toStoreSlots(in)
	want := store.SlotState{Current: 2, Max: 5}
	if out["slot-1"] != want {
		t.Fatalf("toStoreSlots(%v) = %v, want slot-1 = %v", in, out, want)
	}
}

func TestRegisterToState_SeedsIdle(t *testing.T) {
	reg := Frame{
		Type:          "register",
		MailboxID:     "mailbox-1",
		SlotOccupancy: map[string]SlotState{"slot-1": {Current: 0, Max: 5}},
	}
	state := registerToState(reg)
	if state.Status != "idle" {
		t.Errorf("registerToState status = %q, want idle", state.Status)
	}
	if state.SlotOccupancy["slot-1"] != (store.SlotState{Current: 0, Max: 5}) {
		t.Errorf("unexpected slot occupancy: %v", state.SlotOccupancy)
	}
}

func TestJSONStatusPayload_OmitsErrorWhenEmpty(t *testing.T) {
	raw, err := jsonStatusPayload(Frame{Type: "status", JobID: "job-1", Status: "delivered"})
	if err != nil {
		t.Fatalf("jsonStatusPayload: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := asMap["error"]; present {
		t.Errorf("expected no error field for a delivered status, got %s", raw)
	}
	if _, present := asMap["job_id"]; present {
		t.Errorf("statusPayload should not leak job_id (caller already knows it from the channel name), got %s", raw)
	}
	if asMap["status"] != "delivered" {
		t.Errorf("status = %v, want delivered", asMap["status"])
	}
}

func TestJSONStatusPayload_IncludesErrorWhenFailed(t *testing.T) {
	raw, err := jsonStatusPayload(Frame{Type: "status", JobID: "job-1", Status: "failed", Error: "boom"})
	if err != nil {
		t.Fatalf("jsonStatusPayload: %v", err)
	}
	var asMap map[string]any
	json.Unmarshal(raw, &asMap)
	if asMap["error"] != "boom" {
		t.Errorf("expected error=boom, got %s", raw)
	}
}
