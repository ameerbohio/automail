//go:build e2e

// Full-system E2E: one driver test through the entire assembled product
// (testing-plan Part 5 / Goal T8). It drives the real HTTP contract and the
// real browser crypto wire format against a live two-node compose stack
// (scripts/e2e/full.sh brings it up), proving the seams *between* services that
// the per-service unit fakes and the single-node browser E2E (Goal T7) cannot:
//
//  1. Assembled product: encrypt a PDF exactly as portal/lib/encrypt.ts does
//     (AES-256-GCM doc + RSA-OAEP-wrapped key) -> presigned MinIO PUT ->
//     POST /jobs -> the printer decrypts in /dev/shm, "prints" (dev mode), and
//     the status climbs to "delivered" over SSE. The real Redis dispatch and
//     printer-link mTLS socket are in the path, not fakes.
//
//  2. Printer-side wipe (the zero-knowledge RAM-only invariant, end to end):
//     after "delivered", /dev/shm on the printer container holds no job file.
//     This is TestHandleDispatch_DeliversAndWipes promoted to the full stack.
//
//  3. Two-node fan-in / fan-out (roadmap Phase 5 verify, automated): the stack
//     runs two independent cloud nodes; the printer's dial-out socket is pinned
//     to `cloud-server` (owner). The driver submits to and streams from
//     `cloud-server-2` (non-owner):
//     - fan-in : POST /jobs on the non-owner returns status "dispatching",
//     which is only possible if its Publish("mailbox:<id>:dispatch") was
//     received by the OWNER node and relayed down the socket (a non-owner
//     with no live socket would get 0 receivers -> status "queued").
//     - fan-out: the SSE stream on the non-owner receives a *live* status
//     transition (after the initial DB snapshot) ending in "delivered" --
//     that status originated on the owner's socket and crossed Redis.
//
// The crypto/submit/stream/docker primitives live in harness.go (shared with the
// Goal T9 chaos suite). See docker-compose.full.yml for why the two nodes are
// named replicas rather than `--scale cloud-server=2`, and
// docs/study/17-testing-strategy.md.
package e2e

import (
	"bytes"
	"testing"
	"time"
)

func TestFullSystemE2E(t *testing.T) {
	ownerURL := env(t, "E2E_OWNER_URL")       // cloud-server, holds the printer socket
	nonOwnerURL := env(t, "E2E_NONOWNER_URL") // cloud-server-2, never holds the socket

	// --- Resolve the seeded recipient + its printer public key on the OWNER
	// (just to show either node serves reads; the job itself goes to the
	// non-owner via submitEncryptedJob below).
	var recips []recipient
	getJSON(t, ownerURL+"/recipients?q=Testmann", &recips)
	if len(recips) == 0 {
		t.Fatalf("no seeded recipient found (did scripts/e2e/seed.sh run?)")
	}
	recipientID := recips[0].RecipientID

	var pk pubKeyResp
	getJSON(t, ownerURL+"/recipients/"+recipientID+"/public-key", &pk)

	// --- Encrypt exactly as the browser does and assert the wire carries no
	// plaintext, then submit + upload on the NON-OWNER node.
	doc := makePDF()
	_, ciphertext := encryptForPrinter(t, pk.PublicKeyPem, doc)
	if bytes.Contains(ciphertext, []byte(plaintextMarker)) {
		t.Fatal("ciphertext contains the plaintext marker -- encryption did not happen")
	}
	if bytes.HasPrefix(ciphertext, []byte("%PDF-")) {
		t.Fatal("ciphertext carries the PDF magic -- encryption did not happen")
	}

	job := submitEncryptedJob(t, nonOwnerURL)

	// FAN-IN proof: the non-owner returning "dispatching" means its
	// Publish("mailbox:<id>:dispatch") reached a subscriber -- and the only
	// subscriber is the OWNER node holding the printer socket. A non-owner with
	// no live socket of its own would have gotten 0 receivers and enqueued
	// ("queued") instead. So this single value proves cross-node dispatch fan-in.
	if job.Status != "dispatching" {
		t.Fatalf("FAN-IN: POST /jobs on the non-owner returned status %q, want \"dispatching\" "+
			"(the printer must be live+idle so the owner-relayed publish succeeds; check full.sh readiness)", job.Status)
	}
	t.Logf("fan-in OK: job %s submitted on the non-owner dispatched via the owner's socket (status=dispatching)", job.JobID)

	// --- FAN-OUT + assembled-product proof: stream status from the NON-OWNER up
	// to "delivered", and require at least one LIVE transition after the initial
	// DB snapshot (a live event can only have come over Redis from the owner's
	// socket, not from this node's own DB read).
	statuses := streamToTerminal(t, nonOwnerURL, job.JobID, job.GuestToken, 90*time.Second)
	if len(statuses) == 0 {
		t.Fatal("SSE stream on the non-owner produced no events")
	}
	last := statuses[len(statuses)-1]
	if last != "delivered" {
		t.Fatalf("job ended in %q, want \"delivered\" (full trail: %v)", last, statuses)
	}
	if len(statuses) < 2 {
		t.Fatalf("FAN-OUT: only the snapshot event %v arrived on the non-owner -- no live cross-node "+
			"transition witnessed (the print raced ahead of the subscribe; expected on localhost only under load)", statuses)
	}
	t.Logf("fan-out OK: non-owner streamed live status trail %v ending in delivered", statuses)

	// --- Printer-side wipe: /dev/shm holds no job file after delivery.
	assertDevShmClean(t)
	t.Log("printer wipe OK: /dev/shm holds no automail job file after delivered")
}
