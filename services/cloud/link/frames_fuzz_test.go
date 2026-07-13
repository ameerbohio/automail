package link

// Fuzz the printer-link frame parser AND the pure functions the read loop runs
// on a parsed frame (Testing Goal T4 / Part 1). Frames cross the network on the
// mTLS WebSocket, so a hostile frame must never panic our conversion code
// (toStoreSlots / registerToState / jsonStatusPayload).
//
//	go test -run '^$' -fuzz FuzzFrameUnmarshal -fuzztime=30s ./link

import (
	"encoding/json"
	"testing"
)

func FuzzFrameUnmarshal(f *testing.F) {
	f.Add([]byte(`{"type":"register","mailbox_id":"m","slot_occupancy":{"slot-1":{"current":0,"max":5}}}`))
	f.Add([]byte(`{"type":"status","job_id":"j","status":"delivered"}`))
	f.Add([]byte(`{"type":"status","job_id":"j","status":"failed","error":"boom"}`))
	f.Add([]byte(`{"type":"state","slot_occupancy":{}}`))
	f.Add([]byte(`{}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var fr Frame
		if err := json.Unmarshal(data, &fr); err != nil {
			return // malformed JSON correctly rejected
		}
		// Exercise everything the hub does with a parsed frame — none may panic
		// on adversarial contents (huge slot maps, weird status strings, etc.).
		_ = toStoreSlots(fr.SlotOccupancy)
		_ = registerToState(fr)
		if _, err := jsonStatusPayload(fr); err != nil {
			t.Fatalf("jsonStatusPayload failed on an accepted frame: %v", err)
		}
	})
}
