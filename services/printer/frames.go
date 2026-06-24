package main

// SlotInfo mirrors the slot_occupancy entry shape on the wire
// (plans/04-printer-microservice.md). The printer and cloud server are
// separate Go modules/deployables, so this type is duplicated rather than
// shared -- the JSON wire contract is the actual interface between them,
// not a Go type.
type SlotInfo struct {
	Current int `json:"current"`
	Max     int `json:"max"`
}

// Frame is the discriminated union for every printer-link message, same
// shape as the cloud server's link.Frame. Only the fields relevant to
// Type are populated.
type Frame struct {
	Type string `json:"type"`

	// register, state
	MailboxID     string              `json:"mailbox_id,omitempty"`
	SlotOccupancy map[string]SlotInfo `json:"slot_occupancy,omitempty"`
	Status        string              `json:"status,omitempty"` // state: idle|printing; status: printing|delivered|failed

	// status, dispatch
	JobID string `json:"job_id,omitempty"`
	Error string `json:"error,omitempty"`

	// dispatch
	EncryptedKey string `json:"encrypted_key,omitempty"`
	BlobURL      string `json:"blob_url,omitempty"`
}
