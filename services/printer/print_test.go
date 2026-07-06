package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// frameSink collects the frames handleDispatch sends, and runs an optional
// per-frame hook (used to assert ordering, e.g. that the tmpfs file is
// already gone by the time "delivered" is sent).
type frameSink struct {
	mu     sync.Mutex
	frames []Frame
	onSend func(Frame)
}

func (s *frameSink) send(f Frame) {
	s.mu.Lock()
	s.frames = append(s.frames, f)
	s.mu.Unlock()
	if s.onSend != nil {
		s.onSend(f)
	}
}

func (s *frameSink) types() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.frames))
	for i, f := range s.frames {
		out[i] = f.Type + "/" + f.Status
	}
	return out
}

// makeEncryptedJob builds a real dispatch payload for a PDF: AES-256-GCM
// ciphertext ([IV||ct+tag]) plus the AES key RSA-OAEP-wrapped to pub and
// base64'd, exactly as the sender portal produces.
func makeEncryptedJob(t *testing.T, pub *rsa.PublicKey, pdf []byte) (blob []byte, encKeyB64 string) {
	t.Helper()
	aesKey := make([]byte, 32)
	rand.Read(aesKey)

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	iv := make([]byte, gcm.NonceSize())
	rand.Read(iv)
	blob = append(append([]byte{}, iv...), gcm.Seal(nil, iv, pdf, nil)...)

	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, aesKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	return blob, base64.StdEncoding.EncodeToString(wrapped)
}

// swapSeams points the package-level seams at test doubles and returns a
// restore func. tmpfsDir goes to a temp dir so the "plaintext only on
// tmpfs, then unlinked" property is observable without /dev/shm.
func swapSeams(t *testing.T) (dir string, restore func()) {
	t.Helper()
	dir = t.TempDir()
	origTmpfs, origFetch, origPrint, origAfter := tmpfsDir, fetchBlob, printDocument, afterDecrypt
	tmpfsDir = dir
	return dir, func() {
		tmpfsDir, fetchBlob, printDocument, afterDecrypt = origTmpfs, origFetch, origPrint, origAfter
	}
}

func TestHandleDispatch_DeliversAndWipes(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pk := &printerKey{rsa: priv}
	pdf := []byte("%PDF-1.4\nHello from Automail\n%%EOF")
	blob, encB64 := makeEncryptedJob(t, &priv.PublicKey, pdf)

	dir, restore := swapSeams(t)
	defer restore()

	const jobID = "job-deliver"
	path := filepath.Join(dir, "automail-"+jobID+".pdf")

	fetchBlob = func(_ context.Context, _ string) ([]byte, error) { return blob, nil }

	var printedPath string
	printDocument = func(_ /*printerName*/, p string) error { printedPath = p; return nil }

	var capturedPlain []byte
	var plainRef, keyRef []byte
	afterDecrypt = func(_ string, plainPDF, rawAESKey []byte) {
		capturedPlain = append([]byte{}, plainPDF...) // copy content before it's wiped
		plainRef, keyRef = plainPDF, rawAESKey        // keep refs to check wiping later
	}

	sink := &frameSink{onSend: func(f Frame) {
		if f.Type == "status" && f.Status == "delivered" {
			// Ordering guarantee: the tmpfs plaintext must be unlinked
			// BEFORE "delivered" is announced.
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("tmpfs file %s still present when 'delivered' was sent (err=%v)", path, err)
			}
		}
	}}

	state := newSlotState()
	frame := Frame{Type: "dispatch", JobID: jobID, EncryptedKey: encB64, BlobURL: "https://minio/blob"}
	handleDispatch(context.Background(), frame, config{DevMode: false, PrinterName: "TestPrinter"}, state, pk, sink.send)

	// Frame sequence: printing -> delivered -> state.
	if got := sink.types(); len(got) != 3 || got[0] != "status/printing" || got[1] != "status/delivered" || got[2] != "state/idle" {
		t.Fatalf("frame sequence = %v, want [status/printing status/delivered state/idle]", got)
	}
	// Decryption produced the original PDF.
	if string(capturedPlain) != string(pdf) {
		t.Fatalf("decrypted plaintext = %q, want %q", capturedPlain, pdf)
	}
	// lp was invoked with the tmpfs path.
	if printedPath != path {
		t.Fatalf("printDocument path = %q, want %q", printedPath, path)
	}
	// Nothing left on tmpfs.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("tmpfs file still exists after delivery: %v", err)
	}
	// Sensitive buffers were zeroed.
	assertZeroed(t, "plainPDF", plainRef)
	assertZeroed(t, "rawAESKey", keyRef)
	// Slot occupancy grew by one and was reported in the state frame.
	if got := lastStateSlots(sink)["slot-1"].Current; got != 1 {
		t.Fatalf("slot-1 current = %d, want 1", got)
	}
}

func TestHandleDispatch_DevModeSkipsLP(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	pk := &printerKey{rsa: priv}
	blob, encB64 := makeEncryptedJob(t, &priv.PublicKey, []byte("dev pdf"))

	dir, restore := swapSeams(t)
	defer restore()

	fetchBlob = func(_ context.Context, _ string) ([]byte, error) { return blob, nil }
	printDocument = func(_, _ string) error {
		t.Error("printDocument (lp) must not run in DEV_MODE")
		return nil
	}

	sink := &frameSink{}
	state := newSlotState()
	const jobID = "job-dev"
	frame := Frame{Type: "dispatch", JobID: jobID, EncryptedKey: encB64, BlobURL: "x"}
	handleDispatch(context.Background(), frame, config{DevMode: true}, state, pk, sink.send)

	if got := sink.types(); len(got) < 2 || got[1] != "status/delivered" {
		t.Fatalf("dev-mode frames = %v, want a delivered frame", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "automail-"+jobID+".pdf")); !os.IsNotExist(err) {
		t.Fatalf("tmpfs file should be removed even in dev mode: %v", err)
	}
}

func TestHandleDispatch_DecryptFailureReportsFailed(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	pk := &printerKey{rsa: priv}

	dir, restore := swapSeams(t)
	defer restore()

	// A validly-base64'd but undecryptable encrypted_key (random bytes the
	// size of a 2048-bit OAEP ciphertext) makes RSA unwrap fail.
	junk := make([]byte, 256)
	rand.Read(junk)
	encB64 := base64.StdEncoding.EncodeToString(junk)

	fetchBlob = func(_ context.Context, _ string) ([]byte, error) { return []byte("ciphertext"), nil }
	printDocument = func(_, _ string) error { t.Error("lp must not run when decryption fails"); return nil }

	sink := &frameSink{}
	const jobID = "job-fail"
	frame := Frame{Type: "dispatch", JobID: jobID, EncryptedKey: encB64, BlobURL: "x"}
	handleDispatch(context.Background(), frame, config{DevMode: false}, newSlotState(), pk, sink.send)

	got := sink.types()
	if len(got) != 2 || got[0] != "status/printing" || got[1] != "status/failed" {
		t.Fatalf("frames = %v, want [status/printing status/failed]", got)
	}
	// The generic wire error must not leak the failing stage.
	if last := sink.frames[len(sink.frames)-1]; last.Error != "processing failed" {
		t.Fatalf("failed frame error = %q, want generic 'processing failed'", last.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "automail-"+jobID+".pdf")); !os.IsNotExist(err) {
		t.Fatalf("no tmpfs file should exist after a decrypt failure: %v", err)
	}
}

func assertZeroed(t *testing.T, name string, b []byte) {
	t.Helper()
	if len(b) == 0 {
		t.Fatalf("%s: captured slice is empty; wipe assertion is meaningless", name)
	}
	for i, v := range b {
		if v != 0 {
			t.Fatalf("%s: byte %d = %d, want 0 (buffer not wiped)", name, i, v)
		}
	}
}

func lastStateSlots(s *frameSink) map[string]SlotInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.frames) - 1; i >= 0; i-- {
		if s.frames[i].Type == "state" {
			return s.frames[i].SlotOccupancy
		}
	}
	return nil
}
