package main

import (
	"encoding/json"
	"testing"
)

func TestFrame_RegisterRoundTrip(t *testing.T) {
	in := Frame{
		Type:          "register",
		MailboxID:     "mailbox-123",
		SlotOccupancy: map[string]SlotInfo{"slot-1": {Current: 2, Max: 5}},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out Frame
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Type != "register" || out.MailboxID != "mailbox-123" {
		t.Fatalf("got %+v", out)
	}
	if out.SlotOccupancy["slot-1"] != (SlotInfo{Current: 2, Max: 5}) {
		t.Fatalf("slot occupancy mismatch: %+v", out.SlotOccupancy)
	}
	// job_id/blob_url etc. shouldn't appear in a register frame's JSON.
	if string(raw) == "" {
		t.Fatal("expected non-empty JSON")
	}
	var asMap map[string]any
	json.Unmarshal(raw, &asMap)
	for _, omitted := range []string{"job_id", "blob_url", "encrypted_key", "error"} {
		if _, present := asMap[omitted]; present {
			t.Errorf("register frame JSON unexpectedly includes %q: %s", omitted, raw)
		}
	}
}

func TestFrame_DispatchUnmarshal(t *testing.T) {
	raw := []byte(`{
		"type": "dispatch",
		"job_id": "11111111-1111-1111-1111-111111111111",
		"encrypted_key": "YmFzZTY0Y2lwaGVydGV4dA==",
		"blob_url": "https://minio.internal/automail/blobs/abc?X-Amz-Signature=xyz"
	}`)
	var frame Frame
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if frame.Type != "dispatch" {
		t.Errorf("Type = %q, want dispatch", frame.Type)
	}
	if frame.JobID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("JobID = %q", frame.JobID)
	}
	if frame.BlobURL == "" || frame.EncryptedKey == "" {
		t.Errorf("expected blob_url and encrypted_key to be populated, got %+v", frame)
	}
}

func TestFrame_StatusFailedIncludesError(t *testing.T) {
	in := Frame{Type: "status", JobID: "job-1", Status: "failed", Error: "blob fetch failed"}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if asMap["error"] != "blob fetch failed" {
		t.Errorf("expected error field in JSON, got %s", raw)
	}
}
