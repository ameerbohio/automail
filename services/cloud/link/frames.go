// Package link implements the printer-link protocol: the persistent mTLS
// WebSocket that printer microservices dial out to the cloud server and
// hold open (plans/04-printer-microservice.md, plans/09-api-contracts.md
// "Internal Link"). This file defines the JSON frame shapes shared by
// both directions; hub.go and registry.go implement the connection
// lifecycle around them.
package link

import (
	"encoding/json"

	"automail/cloud/store"
)

// SlotState mirrors store.SlotState (mailbox:<id>:state cache shape) but
// is declared independently here -- the wire frame is the contract; the
// Redis cache format happens to match it today but the two are allowed to
// diverge without changing this file.
type SlotState struct {
	Current int `json:"current"`
	Max     int `json:"max"`
}

// Frame is the discriminated union for every printer-link message. Only
// the fields relevant to Type are populated; the rest stay at zero value
// and are omitted from JSON output by their `omitempty` tags.
//
// Printer -> cloud: register, state, status
// Cloud -> printer: dispatch
type Frame struct {
	Type string `json:"type"`

	// register, state
	MailboxID     string               `json:"mailbox_id,omitempty"`
	SlotOccupancy map[string]SlotState `json:"slot_occupancy,omitempty"`
	Status        string               `json:"status,omitempty"` // state: idle|printing; status: printing|delivered|failed

	// status, dispatch
	JobID string `json:"job_id,omitempty"`
	Error string `json:"error,omitempty"` // status, only set when Status == "failed"

	// dispatch
	EncryptedKey string `json:"encrypted_key,omitempty"`
	BlobURL      string `json:"blob_url,omitempty"`
}

// toStoreSlots converts the wire-frame slot map to store.SlotState. The
// two types are structurally identical today; this conversion is the
// seam that lets the wire contract and the cache shape diverge later
// without a breaking change on either side.
func toStoreSlots(slots map[string]SlotState) map[string]store.SlotState {
	out := make(map[string]store.SlotState, len(slots))
	for id, s := range slots {
		out[id] = store.SlotState{Current: s.Current, Max: s.Max}
	}
	return out
}

// registerToState turns a register frame into the cache shape seeded at
// mailbox:<id>:state on connect. A freshly-registered printer hasn't sent
// a status word yet, so it's seeded "idle" -- the first real state frame
// (sent immediately after register per plans/04-printer-microservice.md)
// overwrites this with the printer's actual status.
func registerToState(reg Frame) store.PrinterState {
	return store.PrinterState{Status: "idle", SlotOccupancy: toStoreSlots(reg.SlotOccupancy)}
}

// statusPayload is what gets published to job:<id>:status for the SSE
// relay (Phase 5). Deliberately narrower than the full link.Frame -- a
// sender's browser has no business seeing job_id/type framing details
// that only make sense on the printer-link wire.
type statusPayload struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func jsonStatusPayload(frame Frame) ([]byte, error) {
	return json.Marshal(statusPayload{Status: frame.Status, Error: frame.Error})
}
