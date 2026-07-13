package main

// Fuzz the printer-link frame parser (Testing Goal T4 / Part 1). Frames arrive
// over the network (the mTLS WebSocket), so unmarshalling a hostile frame must
// never panic and re-marshalling a parsed frame must stay well-formed.
//
//	go test -run '^$' -fuzz FuzzFrameUnmarshal -fuzztime=30s .

import (
	"encoding/json"
	"testing"
)

func FuzzFrameUnmarshal(f *testing.F) {
	f.Add([]byte(`{"type":"dispatch","job_id":"j","encrypted_key":"YQ==","blob_url":"https://m/b"}`))
	f.Add([]byte(`{"type":"register","mailbox_id":"m","slot_occupancy":{"slot-1":{"current":0,"max":5}}}`))
	f.Add([]byte(`{"type":"state","status":"idle"}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, data []byte) {
		var fr Frame
		if err := json.Unmarshal(data, &fr); err != nil {
			return // malformed JSON is correctly rejected
		}
		// A frame we accepted must re-serialize without error (no NaN/Inf, no
		// unencodable state reachable from arbitrary input).
		if _, err := json.Marshal(fr); err != nil {
			t.Fatalf("re-marshal of accepted frame failed: %v", err)
		}
	})
}
