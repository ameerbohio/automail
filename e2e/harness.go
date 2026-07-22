//go:build e2e || chaos

// Shared harness for the full-system E2E (Goal T8, build tag `e2e`) and the
// resilience/chaos suite (Goal T9, build tag `chaos`). Both drive the product's
// real HTTP contract and the browser's exact crypto wire format -- with nothing
// but the Go standard library -- against a live two-node compose stack that
// scripts/e2e/{full,chaos}.sh bring up. Keeping the primitives here (encrypt,
// submit, stream, docker control, /dev/shm inspection) lets the two suites stay
// DRY without either importing the cloud/printer modules.
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

// env reads a required environment knob set by scripts/e2e/{full,chaos}.sh; a
// missing one is a harness bug, not a product failure, so fail loudly.
func env(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("required env %s is unset (run via `make test-e2e-full` / `make chaos`, not bare `go test`)", key)
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

// createJobRequest mirrors handlers.createJobRequest's JSON contract.
type createJobRequest struct {
	EncryptedKey string `json:"encrypted_key"`
	BlobRef      string `json:"blob_ref"`
	RecipientID  string `json:"recipient_id"`
	PageCount    int32  `json:"page_count"`
}

// submitEncryptedJob runs the whole guest submission the browser would: resolve
// the seeded recipient + its printer public key, encrypt a marker PDF exactly as
// portal/lib/encrypt.ts does, PUT the ciphertext straight to object storage
// (the cloud never sees the blob bytes), then POST /jobs. baseURL selects which
// cloud node handles the submission. Returns the created job (job_id, the
// server-assigned status, and the guest token for the SSE stream).
func submitEncryptedJob(t *testing.T, baseURL string) createJobResp {
	t.Helper()

	var recips []recipient
	getJSON(t, baseURL+"/recipients?q=Testmann", &recips)
	if len(recips) == 0 {
		t.Fatalf("no seeded recipient found (did scripts/e2e/seed.sh run?)")
	}
	recipientID := recips[0].RecipientID

	var pk pubKeyResp
	getJSON(t, baseURL+"/recipients/"+recipientID+"/public-key", &pk)

	doc := makePDF()
	encKeyB64, ciphertext := encryptForPrinter(t, pk.PublicKeyPem, doc)
	if bytes.Contains(ciphertext, []byte(plaintextMarker)) || bytes.HasPrefix(ciphertext, []byte("%PDF-")) {
		t.Fatal("ciphertext still carries plaintext -- encryption did not happen")
	}

	var up uploadURLResp
	if code := postJSON(t, baseURL+"/jobs/upload-url",
		map[string]string{"recipient_id": recipientID, "filename": "letter.pdf"}, &up); code != http.StatusOK {
		t.Fatalf("POST /jobs/upload-url: status %d", code)
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

	var job createJobResp
	code := postJSON(t, baseURL+"/jobs", createJobRequest{
		EncryptedKey: encKeyB64,
		BlobRef:      up.BlobRef,
		RecipientID:  recipientID,
		PageCount:    1,
	}, &job)
	if code != http.StatusAccepted {
		t.Fatalf("POST /jobs: status %d, body %+v", code, job)
	}
	if job.JobID == "" || job.GuestToken == "" {
		t.Fatalf("POST /jobs returned no job id / guest token: %+v", job)
	}
	return job
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
		t.Fatalf("open SSE stream: %v", err)
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

// composeArgs is the docker-compose invocation both suites use to reach the
// running two-node stack (same project scripts/e2e/{full,chaos}.sh brought up).
// E2E_REPO_ROOT pins the project directory so `exec`/`stop`/`start`/`restart`
// target the same containers regardless of the test's cwd.
func composeArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	root := env(t, "E2E_REPO_ROOT")
	base := []string{
		"compose",
		"--project-directory", root,
		"-f", root + "/docker-compose.yml",
		"-f", root + "/docker-compose.full.yml",
	}
	return append(base, extra...)
}

// dockerCompose runs a docker-compose subcommand against the running stack and
// returns its combined output, failing the test on a non-zero exit.
func dockerCompose(t *testing.T, extra ...string) string {
	t.Helper()
	out, err := exec.Command("docker", composeArgs(t, extra...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", extra, err, out)
	}
	return string(out)
}

// assertDevShmClean execs into the printer container and asserts no automail job
// file remains under /dev/shm -- the plaintext PDF was unlinked before the
// delivered callback and never touched disk elsewhere.
func assertDevShmClean(t *testing.T) {
	t.Helper()
	out := dockerCompose(t, "exec", "-T", "printer", "sh", "-c", "ls -A /dev/shm 2>/dev/null || true")
	listing := strings.TrimSpace(out)
	for _, name := range strings.Fields(listing) {
		if strings.HasPrefix(name, "automail-") || strings.HasSuffix(name, ".pdf") {
			t.Fatalf("plaintext left in /dev/shm after delivered: %q (full listing: %q)", name, listing)
		}
	}
	t.Logf("/dev/shm listing after delivered: %q", listing)
}
