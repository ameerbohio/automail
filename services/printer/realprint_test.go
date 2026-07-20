package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"os"
	"path/filepath"
	"testing"
)

// TestRealPrint_EndToEnd exercises the real Phase 10 print path against a
// physical printer: RSA-OAEP unwrap -> AES-256-GCM decrypt -> write to the real
// tmpfs (/dev/shm) -> real `lp -d $PRINTER_NAME` -> unlink before "delivered".
// Only fetchBlob is stubbed (to hand over ciphertext without MinIO); the print
// call and tmpfs are the genuine article.
//
// It is GATED so normal `go test` and CI never run it (it consumes paper):
//
//	AUTOMAIL_REAL_PRINT=1 \
//	PRINTER_NAME="Canon MF240 Series (Copy 1)" \
//	AUTOMAIL_TEST_PDF=/path/to/real.pdf \
//	PATH="/home/ameer/automail-print-bridge:$PATH" \
//	go test ./ -run TestRealPrint_EndToEnd -v
//
// On this WSL2 dev box the on-PATH `lp` is the SumatraPDF->Windows bridge; on
// the production host it is the real CUPS `lp`. The code under test is identical.
func TestRealPrint_EndToEnd(t *testing.T) {
	if os.Getenv("AUTOMAIL_REAL_PRINT") != "1" {
		t.Skip("gated: set AUTOMAIL_REAL_PRINT=1 (+PRINTER_NAME, AUTOMAIL_TEST_PDF) to print real paper")
	}
	printer := os.Getenv("PRINTER_NAME")
	if printer == "" {
		t.Fatal("PRINTER_NAME must be set")
	}
	pdf, err := os.ReadFile(os.Getenv("AUTOMAIL_TEST_PDF"))
	if err != nil {
		t.Fatalf("read AUTOMAIL_TEST_PDF: %v", err)
	}

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pk := &printerKey{rsa: priv}
	blob, encB64 := makeEncryptedJob(t, &priv.PublicKey, pdf)

	origFetch := fetchBlob
	fetchBlob = func(_ context.Context, _ string) ([]byte, error) { return blob, nil }
	defer func() { fetchBlob = origFetch }()

	const jobID = "realprint"
	path := filepath.Join(tmpfsDir, "automail-"+jobID+".pdf")

	sink := &frameSink{}
	frame := Frame{Type: "dispatch", JobID: jobID, EncryptedKey: encB64, BlobURL: "https://minio/blob"}
	handleDispatch(context.Background(), frame, config{DevMode: false, PrinterName: printer}, newSlotState("slot-1"), pk, sink.send)

	got := sink.types()
	if len(got) < 2 || got[1] != "status/delivered" {
		t.Fatalf("frames = %v; job did not reach 'delivered' (print failed)", got)
	}
	// Phase 10 invariant: nothing left on the real tmpfs after delivery.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("tmpfs plaintext %s still present after delivery: %v (want removed)", path, err)
	}
	t.Logf("delivered; %s unlinked; paper printing on %q", path, printer)
}
