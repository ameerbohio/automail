package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
)

// slotState is the in-process map of slot_id -> occupancy, updated after
// each delivery and reflected in the next state frame
// (plans/04-printer-microservice.md "Keepalive and State Reporting").
// Phase 3 has no real slot hardware to read, so it starts with one fixed
// demo slot and never actually changes its counts -- Phase 6 (real print
// pipeline) is what increments/decrements this for real.
type slotState struct {
	mu     sync.Mutex
	status string // idle | printing
	slots  map[string]SlotInfo
}

func newSlotState() *slotState {
	return &slotState{
		status: "idle",
		slots: map[string]SlotInfo{
			"slot-1": {Current: 0, Max: 5},
		},
	}
}

func (s *slotState) snapshot() (status string, slots map[string]SlotInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make(map[string]SlotInfo, len(s.slots))
	for k, v := range s.slots {
		cp[k] = v
	}
	return s.status, cp
}

// handleDispatch processes one dispatch frame asynchronously, per
// plans/04-printer-microservice.md "Processing a dispatch frame": handed
// to its own goroutine by the caller so the read loop stays free for
// ping/pong and further frames. send delivers status frames back up the
// socket.
//
// Dev-mode stub (DEV_MODE=true, the only mode Phase 3 implements): skip
// RSA/AES decrypt and CUPS entirely. HEAD the blob_url to prove it's a
// valid pre-signed URL, write a placeholder file to /tmp, delete it
// immediately, then report delivered. Real decrypt+print lands in
// Phase 6 (plans/10-implementation-roadmap.md).
func handleDispatch(ctx context.Context, frame Frame, devMode bool, send func(Frame)) {
	send(Frame{Type: "status", JobID: frame.JobID, Status: "printing"})

	if !devMode {
		// Phase 6 implements the real decrypt/print pipeline. Phase 3
		// only ships the dev-mode path; refuse rather than silently
		// pretend to print in a mode this phase doesn't implement.
		log.Printf("job %s: DEV_MODE=false not yet implemented (Phase 6)", frame.JobID)
		send(Frame{Type: "status", JobID: frame.JobID, Status: "failed", Error: "printer not yet implemented (non-dev mode)"})
		return
	}

	if err := verifyBlobURL(ctx, frame.BlobURL); err != nil {
		log.Printf("job %s: blob_url HEAD check failed: %v", frame.JobID, err)
		send(Frame{Type: "status", JobID: frame.JobID, Status: "failed", Error: "blob fetch failed"})
		return
	}

	path := fmt.Sprintf("/tmp/automail-dev-%s.txt", frame.JobID)
	contents := []byte("automail dev-mode stub: would print job " + frame.JobID + "\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		log.Printf("job %s: dev: write stub file: %v", frame.JobID, err)
		send(Frame{Type: "status", JobID: frame.JobID, Status: "failed", Error: "dev stub write failed"})
		return
	}
	log.Printf("job %s: dev: would print (wrote %s)", frame.JobID, path)
	if err := os.Remove(path); err != nil {
		// Not fatal -- the job still "delivered" from the printer's
		// perspective. But this is exactly the unlink step that, with
		// real plaintext (Phase 6), is not allowed to be skipped or
		// deferred, so a failure here is logged loudly even in dev mode.
		log.Printf("job %s: dev: failed to remove stub file %s: %v", frame.JobID, path, err)
	} else {
		log.Printf("job %s: dev: deleted %s", frame.JobID, path)
	}

	send(Frame{Type: "status", JobID: frame.JobID, Status: "delivered"})
}

// verifyBlobURL issues a HEAD request against the pre-signed MinIO read
// URL to confirm it resolves before "processing" the dummy job -- the
// dev-mode substitute for actually fetching ciphertext into RAM
// (plans/04-printer-microservice.md "Fetch blob from blob_url (HEAD
// request to verify URL is valid in dev)").
func verifyBlobURL(ctx context.Context, blobURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, blobURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("blob_url returned status %d", resp.StatusCode)
	}
	return nil
}
