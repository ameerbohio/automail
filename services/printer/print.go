package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

// slotState is the in-process map of slot_id -> occupancy, updated after
// each delivery and reflected in the next state frame
// (plans/04-printer-microservice.md "Keepalive and State Reporting").
// The demo unit has one fixed slot; recordDelivery bumps its count after a
// successful print, which the caller pushes up as a state frame.
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
	return s.status, s.copySlots()
}

// recordDelivery increments the given slot's occupancy (capped at max --
// dispatch shouldn't send to a full slot, but the cap keeps the count
// honest if it ever does) and returns a fresh snapshot for the state frame.
func (s *slotState) recordDelivery(slotID string) (status string, slots map[string]SlotInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := s.slots[slotID]
	if info.Current < info.Max {
		info.Current++
	}
	s.slots[slotID] = info
	return s.status, s.copySlots()
}

// copySlots returns a defensive copy so callers can't mutate the map the
// mutex protects. Caller must hold s.mu.
func (s *slotState) copySlots() map[string]SlotInfo {
	cp := make(map[string]SlotInfo, len(s.slots))
	for k, v := range s.slots {
		cp[k] = v
	}
	return cp
}

// tmpfsDir is where decrypted PDFs are written before printing. It MUST be
// RAM-backed (/dev/shm) so plaintext never touches persistent disk
// (plans/02-security.md; plans/04 "Security Properties"). A package var so
// tests can point it at t.TempDir().
var tmpfsDir = "/dev/shm"

// printDocument hands a file to CUPS. A package var so tests exercise the
// pipeline without a real printer; the real path shells out to lp.
var printDocument = func(printerName, path string) error {
	return exec.Command("lp", "-d", printerName, path).Run()
}

// fetchBlob downloads the ciphertext blob into RAM from its pre-signed
// MinIO URL. A package var so tests can inject ciphertext without a server.
var fetchBlob = func(ctx context.Context, blobURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, blobURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("blob fetch returned status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// afterDecrypt is a test-only seam invoked with the decrypted material just
// before it is written/printed, letting a test assert the plaintext is
// correct and later confirm the same backing arrays were zeroed. nil in
// production.
var afterDecrypt func(jobID string, plainPDF, rawAESKey []byte)

// handleDispatch processes one dispatch frame asynchronously
// (plans/04-printer-microservice.md "Processing a dispatch frame"): it is
// handed its own goroutine by the read loop so keepalive and further frames
// keep flowing. It reports "printing", runs the decrypt/print/wipe
// pipeline, then reports "delivered" and the new slot occupancy -- or
// "failed" with a deliberately generic message.
func handleDispatch(ctx context.Context, frame Frame, cfg config, state *slotState, key *printerKey, send func(Frame)) {
	send(Frame{Type: "status", JobID: frame.JobID, Status: "printing"})

	if err := processJob(ctx, frame, cfg, key); err != nil {
		// The wire error is intentionally generic: the specific stage (RSA
		// unwrap vs GCM auth vs padding) must not leak, or a malicious blob
		// could turn "failed" into a padding/decryption oracle. The real
		// cause is logged locally only, and carries no document content.
		log.Printf("job %s: processing failed: %v", frame.JobID, err)
		send(Frame{Type: "status", JobID: frame.JobID, Status: "failed", Error: "processing failed"})
		return
	}

	send(Frame{Type: "status", JobID: frame.JobID, Status: "delivered"})

	status, slots := state.recordDelivery("slot-1")
	send(Frame{Type: "state", Status: status, SlotOccupancy: slots})
}

// processJob runs the decrypt -> print -> wipe pipeline for one job. It
// returns nil ONLY after the tmpfs plaintext has been unlinked and every
// sensitive buffer zeroed -- so a nil return is the caller's license to
// send "delivered". The deferred wipes run as this function returns, i.e.
// strictly before the caller's "delivered" frame. Every error path also
// leaves no plaintext behind: buffers are zeroed by the same defers and any
// tmpfs file is removed.
func processJob(ctx context.Context, frame Frame, cfg config, key *printerKey) error {
	// Registered first, so it runs LAST (defers are LIFO): every zeroBytes
	// defer below has already wiped its buffer by the time this GC hint
	// fires -- i.e. "zero everything, then GC" (plans/04 "In-Memory Zeroing
	// Pattern"). The GC is only a best-effort hint; the zeroing is the real
	// guarantee.
	defer runtime.GC()

	// 1. Fetch ciphertext into RAM.
	ciphertextBlob, err := fetchBlob(ctx, frame.BlobURL)
	if err != nil {
		return fmt.Errorf("fetch blob: %w", err)
	}
	defer zeroBytes(ciphertextBlob)

	// 2. Base64-decode the RSA-wrapped AES key (the cloud sends it as
	//    standard-encoding base64 in the dispatch frame).
	encryptedKey, err := base64.StdEncoding.DecodeString(frame.EncryptedKey)
	if err != nil {
		return fmt.Errorf("decode encrypted_key: %w", err)
	}
	defer zeroBytes(encryptedKey)

	// 3. RSA-OAEP unwrap the per-job AES key.
	rawAESKey, err := key.DecryptAESKey(encryptedKey)
	if err != nil {
		return fmt.Errorf("unwrap AES key: %w", err)
	}
	defer zeroBytes(rawAESKey)

	// 4. AES-256-GCM decrypt the document (IV = first 12 bytes).
	plainPDF, err := DecryptDocument(ciphertextBlob, rawAESKey)
	if err != nil {
		return fmt.Errorf("decrypt document: %w", err)
	}
	defer zeroBytes(plainPDF)

	if afterDecrypt != nil {
		afterDecrypt(frame.JobID, plainPDF, rawAESKey)
	}

	// 5. Write to tmpfs (RAM-backed, 0600). Plaintext never hits real disk.
	path := filepath.Join(tmpfsDir, fmt.Sprintf("automail-%s.pdf", frame.JobID))
	if err := os.WriteFile(path, plainPDF, 0o600); err != nil {
		return fmt.Errorf("write tmpfs: %w", err)
	}

	// 6. Print. DEV_MODE skips only the physical lp call; steps 1-5 (incl.
	//    real decryption) and the unlink below still run.
	if cfg.DevMode {
		log.Printf("job %s: dev: would print %s (%d bytes)", frame.JobID, path, len(plainPDF))
	} else if err := printDocument(cfg.PrinterName, path); err != nil {
		// Remove the plaintext even though the print failed -- it must not
		// outlive this function on any path.
		if rmErr := os.Remove(path); rmErr != nil {
			log.Printf("job %s: WARNING could not remove tmpfs after print failure: %v", frame.JobID, rmErr)
		}
		return fmt.Errorf("print: %w", err)
	}

	// 7. Unlink the tmpfs plaintext BEFORE the caller reports "delivered".
	//    A failed unlink must surface as an error so the job is NOT reported
	//    delivered -- plaintext would otherwise remain on the tmpfs.
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove tmpfs: %w", err)
	}

	return nil
}
