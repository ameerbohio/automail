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
// See docker-compose.full.yml for why the two nodes are named replicas rather
// than `--scale cloud-server=2`, and docs/study/17-testing-strategy.md.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// plaintextMarker is an in-the-clear string embedded in the test PDF. Nothing
// uploaded to object storage may contain it -- if it did, plaintext escaped the
// encrypt step. Mirrors the browser E2E's PLAINTEXT_MARKER.
const plaintextMarker = "AUTOMAIL_FULLSTACK_PLAINTEXT_DO_NOT_LEAK"

// env reads a required environment knob set by scripts/e2e/full.sh; a missing
// one is a harness bug, not a product failure, so fail loudly.
func env(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("required env %s is unset (run via `make test-e2e-full`, not bare `go test`)", key)
	}
	return v
}

func makePDF() []byte {
	body := strings.Join([]string{
		"%PDF-1.4",
		"1 0 obj << /Type /Catalog /Pages 2 0 R >> endobj",
		"2 0 obj << /Type /Pages /Kids [3 0 R] /Count 1 >> endobj",
		"3 0 obj << /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 4 0 R >> endobj",
		"4 0 obj << /Length 60 >> stream",
		"BT /F1 12 Tf 20 100 Td (" + plaintextMarker + ") Tj ET",
		"endstream endobj",
		"trailer << /Root 1 0 R >>",
		"%%EOF",
	}, "\n")
	return []byte(body)
}

// encryptForPrinter reproduces portal/lib/encrypt.ts byte-for-byte: a fresh
// AES-256 key encrypts the document with GCM (wire = 12-byte IV || ct+tag), and
// the key is RSA-OAEP(SHA-256, empty label)-wrapped to the recipient's public
// key. Proven equivalent to the browser by the T2 crypto contract; the printer
// decrypts it with crypto.go's DecryptAESKey + DecryptDocument.
func encryptForPrinter(t *testing.T, pubPEM string, doc []byte) (encryptedKeyB64 string, ciphertext []byte) {
	t.Helper()
	block, _ := pem.Decode([]byte(pubPEM))
	if block == nil {
		t.Fatalf("recipient public_key_pem has no PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse recipient SPKI public key: %v", err)
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok {
		t.Fatalf("recipient key is %T, want *rsa.PublicKey", parsed)
	}

	aesKey := make([]byte, 32)
	if _, err := rand.Read(aesKey); err != nil {
		t.Fatal(err)
	}
	encKey, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, aesKey, nil)
	if err != nil {
		t.Fatalf("RSA-OAEP wrap: %v", err)
	}
	c, err := aes.NewCipher(aesKey)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(c)
	if err != nil {
		t.Fatal(err)
	}
	iv := make([]byte, gcm.NonceSize()) // 12 bytes, matches the browser
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	ciphertext = append(append([]byte(nil), iv...), gcm.Seal(nil, iv, doc, nil)...)
	return base64.StdEncoding.EncodeToString(encKey), ciphertext
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // url is a test-controlled localhost endpoint
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: status %d: %s", url, resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func postJSON(t *testing.T, url string, body, out any) int {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode POST %s (status %d): %v: %s", url, resp.StatusCode, err, raw)
		}
	}
	return resp.StatusCode
}

type recipient struct {
	RecipientID string `json:"recipient_id"`
	DisplayName string `json:"display_name"`
}

type pubKeyResp struct {
	RecipientID  string `json:"recipient_id"`
	PublicKeyPem string `json:"public_key_pem"`
}

type uploadURLResp struct {
	UploadURL string `json:"upload_url"`
	BlobRef   string `json:"blob_ref"`
}

type createJobResp struct {
	JobID      string `json:"job_id"`
	Status     string `json:"status"`
	GuestToken string `json:"guest_token"`
}

func TestFullSystemE2E(t *testing.T) {
	ownerURL := env(t, "E2E_OWNER_URL")       // cloud-server, holds the printer socket
	nonOwnerURL := env(t, "E2E_NONOWNER_URL") // cloud-server-2, never holds the socket

	// --- Resolve the seeded recipient + its printer public key (real contract:
	// the browser learns the key from /recipients/{id}/public-key). Queried on
	// the OWNER just to show either node serves reads; the job goes to the
	// non-owner below.
	var recips []recipient
	getJSON(t, ownerURL+"/recipients?q=Testmann", &recips)
	if len(recips) == 0 {
		t.Fatalf("no seeded recipient found (did scripts/e2e/seed.sh run?)")
	}
	recipientID := recips[0].RecipientID

	var pk pubKeyResp
	getJSON(t, ownerURL+"/recipients/"+recipientID+"/public-key", &pk)

	// --- Encrypt exactly as the browser does.
	doc := makePDF()
	encKeyB64, ciphertext := encryptForPrinter(t, pk.PublicKeyPem, doc)

	// Zero-knowledge on the wire: what we upload must be ciphertext, never the
	// plaintext PDF (the driver-side mirror of the browser E2E's assertion).
	if bytes.Contains(ciphertext, []byte(plaintextMarker)) {
		t.Fatal("ciphertext contains the plaintext marker -- encryption did not happen")
	}
	if bytes.HasPrefix(ciphertext, []byte("%PDF-")) {
		t.Fatal("ciphertext carries the PDF magic -- encryption did not happen")
	}

	// --- Presigned upload URL from the NON-OWNER, then PUT the ciphertext
	// straight to object storage (the cloud server never sees the blob bytes).
	var up uploadURLResp
	if code := postJSON(t, nonOwnerURL+"/jobs/upload-url",
		map[string]string{"recipient_id": recipientID, "filename": "letter.pdf"}, &up); code != http.StatusOK {
		t.Fatalf("POST /jobs/upload-url on non-owner: status %d", code)
	}
	putReq, err := http.NewRequest(http.MethodPut, up.UploadURL, bytes.NewReader(ciphertext))
	if err != nil {
		t.Fatal(err)
	}
	putReq.Header.Set("Content-Type", "application/octet-stream")
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT ciphertext to object storage: %v", err)
	}
	putBody, _ := io.ReadAll(putResp.Body)
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("upload PUT: status %d: %s", putResp.StatusCode, putBody)
	}

	// --- Submit the job on the NON-OWNER node.
	var job createJobResp
	code := postJSON(t, nonOwnerURL+"/jobs", createJobRequest{
		EncryptedKey: encKeyB64,
		BlobRef:      up.BlobRef,
		RecipientID:  recipientID,
		PageCount:    1,
	}, &job)
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs on non-owner: status %d, body %+v", code, job)
	}
	if job.JobID == "" || job.GuestToken == "" {
		t.Fatalf("POST /jobs on non-owner returned no job id / guest token: %+v", job)
	}

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

// createJobRequest mirrors handlers.createJobRequest's JSON contract.
type createJobRequest struct {
	EncryptedKey string `json:"encrypted_key"`
	BlobRef      string `json:"blob_ref"`
	RecipientID  string `json:"recipient_id"`
	PageCount    int32  `json:"page_count"`
}

// streamToTerminal opens GET /jobs/{id}/stream?token=... and returns the ordered
// list of statuses seen until a terminal one (delivered/failed) or the timeout.
// The first element is the handler's initial DB snapshot; any later element
// arrived over the Redis job:<id>:status channel (the fan-out path).
func streamToTerminal(t *testing.T, baseURL, jobID, guestToken string, timeout time.Duration) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	url := fmt.Sprintf("%s/jobs/%s/stream?token=%s", baseURL, jobID, guestToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open SSE stream on non-owner: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("SSE stream: status %d: %s", resp.StatusCode, b)
	}

	var statuses []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // skip blank separators / comments
		}
		var ev struct {
			JobID  string `json:"job_id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(line[len("data:"):])), &ev); err != nil {
			t.Fatalf("malformed SSE data line %q: %v", line, err)
		}
		if ev.JobID != jobID {
			t.Fatalf("SSE event carried job_id %q, want %q (wire format must restore job_id)", ev.JobID, jobID)
		}
		statuses = append(statuses, ev.Status)
		if ev.Status == "delivered" || ev.Status == "failed" {
			return statuses
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading SSE stream (timeout after %s? trail so far %v): %v", timeout, statuses, err)
	}
	return statuses
}

// assertDevShmClean execs into the printer container and asserts no automail job
// file remains under /dev/shm -- the plaintext PDF was unlinked before the
// delivered callback and never touched disk elsewhere.
func assertDevShmClean(t *testing.T) {
	t.Helper()
	root := env(t, "E2E_REPO_ROOT")
	args := []string{
		"compose",
		"--project-directory", root,
		"-f", root + "/docker-compose.yml",
		"-f", root + "/docker-compose.full.yml",
		"exec", "-T", "printer", "sh", "-c", "ls -A /dev/shm 2>/dev/null || true",
	}
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect printer /dev/shm: %v: %s", err, out)
	}
	listing := strings.TrimSpace(string(out))
	for _, name := range strings.Fields(listing) {
		if strings.HasPrefix(name, "automail-") || strings.HasSuffix(name, ".pdf") {
			t.Fatalf("plaintext left in /dev/shm after delivered: %q (full listing: %q)", name, listing)
		}
	}
	t.Logf("/dev/shm listing after delivered: %q", listing)
}
