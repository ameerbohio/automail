package main

// Phase 5 tests for GET /jobs/:id/stream (plans/10-implementation-roadmap.md,
// plans/09-api-contracts.md). These live in package main deliberately: the
// auth matrix must run through the real optionalAuth middleware with real
// RS256 Bearer tokens, in exactly the chain main.go registers -- not a
// reimplementation of it. Postgres is replaced by the fake driver in
// dbfake_test.go; Redis is miniredis, as in link/hub_integration_test.go.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"automail/cloud/db"
	"automail/cloud/handlers"
	"automail/cloud/jwtutil"
	"automail/cloud/link"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// hashGuestTokenForTest mirrors handlers.hashGuestToken (SHA-256, base64
// RawURL) -- recomputed here rather than exported, so the test asserts the
// documented storage format independently of the implementation.
func hashGuestTokenForTest(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// fakeStreamJob is the jobs row the fake DB serves to GetJobForStream.
// SenderID / GuestTokenHash are driver.Values: nil (SQL NULL) or string.
type fakeStreamJob struct {
	ID             string
	SenderID       driver.Value
	GuestTokenHash driver.Value
	Status         string
}

// streamQueries serves GetJobForStream from a static row. job == nil
// simulates "no such job" (zero rows -> sql.ErrNoRows in the handler).
func streamQueries(job *fakeStreamJob) fakeQueryFunc {
	return func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if !strings.HasPrefix(query, "-- name: GetJobForStream") {
			return nil, nil, fmt.Errorf("unexpected query: %.60s", query)
		}
		cols := []string{"id", "sender_id", "guest_token_hash", "status"}
		if job == nil {
			return cols, nil, nil
		}
		return cols, [][]driver.Value{{job.ID, job.SenderID, job.GuestTokenHash, job.Status}}, nil
	}
}

// hubQueries serves the two queries link.Hub.onStatus issues when a
// status frame arrives: UpdateJobStatus (echoes the requested status
// back, as Postgres' RETURNING would) and InsertAuditEvent.
func hubQueries(jobID, mailboxID string) fakeQueryFunc {
	return func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		switch {
		case strings.HasPrefix(query, "-- name: UpdateJobStatus"):
			status, ok := args[0].Value.(string) // SET status = $1 ... WHERE id = $2
			if !ok {
				return nil, nil, fmt.Errorf("UpdateJobStatus: arg 0 is %T, want string", args[0].Value)
			}
			return []string{"id", "mailbox_id", "blob_ref", "status"},
				[][]driver.Value{{jobID, mailboxID, "blobs/test", status}}, nil
		case strings.HasPrefix(query, "-- name: InsertAuditEvent"):
			return nil, nil, nil
		default:
			return nil, nil, fmt.Errorf("unexpected query: %.60s", query)
		}
	}
}

// newStreamNode assembles one "cloud node": a handlers.Server whose
// Queries run against the fake driver, a Redis client on the shared
// miniredis, and the GET /jobs/{id}/stream route behind the real
// optionalAuth middleware -- the same chain main.go builds.
func newStreamNode(t *testing.T, mr *miniredis.Miniredis, jwtPub *rsa.PublicKey, q fakeQueryFunc) *httptest.Server {
	t.Helper()
	sqlDB := sql.OpenDB(fakeConnector{q: q})
	t.Cleanup(func() { sqlDB.Close() })
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	srv := &handlers.Server{Queries: db.New(sqlDB), SQLDB: sqlDB, Redis: rdb}
	mux := http.NewServeMux()
	mux.Handle("GET /jobs/{id}/stream", optionalAuth(jwtPub)(http.HandlerFunc(srv.StreamJob)))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func newJWTKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return key
}

// openStream GETs /jobs/<id>/stream with the given credentials. The
// request context carries a deadline so a stuck stream fails the test
// instead of hanging it.
func openStream(t *testing.T, ctx context.Context, baseURL, jobID, token, bearer string) *http.Response {
	t.Helper()
	url := baseURL + "/jobs/" + jobID + "/stream"
	if token != "" {
		url += "?token=" + token
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// readSSEData reads one SSE event off the stream and returns its data
// payload (the JSON between "data: " and the blank line).
func readSSEData(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	var data string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading SSE stream: %v", err)
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" && data != "" {
			return data
		}
		if after, ok := strings.CutPrefix(line, "data: "); ok {
			data = after
		}
	}
}

// expectStreamClosed asserts the server ended the response body (what
// "connection closes when status reaches delivered or failed" looks like
// from the client side).
func expectStreamClosed(t *testing.T, br *bufio.Reader) {
	t.Helper()
	if _, err := br.ReadString('\n'); !errors.Is(err, io.EOF) {
		t.Fatalf("expected stream closed (EOF), got err=%v", err)
	}
}

// TestStreamJob_AuthMatrix drives every accept/reject combination of the
// contract's two credentials (plans/09-api-contracts.md "GET
// /jobs/:id/stream": owner JWT or guest token) through the real
// optionalAuth middleware. Accepted requests get the SSE snapshot event;
// the job is terminal ('delivered') so the handler closes immediately
// and the whole body is readable synchronously.
func TestStreamJob_AuthMatrix(t *testing.T) {
	key := newJWTKey(t)
	ownerID := uuid.New()
	strangerID := uuid.New()
	jobID := uuid.New().String()
	guestJobID := uuid.New().String()
	const rawToken = "guest-token-raw-value"

	ownerJWT, err := jwtutil.IssueAccessToken(key, ownerID, "sender")
	if err != nil {
		t.Fatalf("issue owner token: %v", err)
	}
	strangerJWT, err := jwtutil.IssueAccessToken(key, strangerID, "sender")
	if err != nil {
		t.Fatalf("issue stranger token: %v", err)
	}

	// An authenticated sender's job: sender_id set, no guest token hash.
	authedJob := &fakeStreamJob{ID: jobID, SenderID: ownerID.String(), GuestTokenHash: nil, Status: "delivered"}
	// A guest job: sender_id NULL, guest token hash set.
	guestJob := &fakeStreamJob{ID: guestJobID, SenderID: nil, GuestTokenHash: hashGuestTokenForTest(rawToken), Status: "delivered"}

	cases := []struct {
		name       string
		job        *fakeStreamJob // row the fake DB serves (nil = no such job)
		jobID      string
		token      string
		bearer     string
		wantStatus int
		wantCode   string // error code for rejects; "" for accepted
	}{
		{"owner JWT accepted", authedJob, jobID, "", ownerJWT, http.StatusOK, ""},
		{"wrong sender JWT gets 404", authedJob, jobID, "", strangerJWT, http.StatusNotFound, "NOT_FOUND"},
		{"valid guest token accepted", guestJob, guestJobID, rawToken, "", http.StatusOK, ""},
		{"bad guest token gets 403", guestJob, guestJobID, "wrong-token", "", http.StatusForbidden, "GUEST_TOKEN_INVALID"},
		{"no credentials gets 401", authedJob, jobID, "", "", http.StatusUnauthorized, "UNAUTHORIZED"},
		{"garbage bearer without token gets 401", authedJob, jobID, "", "not-a-jwt", http.StatusUnauthorized, "UNAUTHORIZED"},
		{"stranger JWT on a guest job gets 404", guestJob, guestJobID, "", strangerJWT, http.StatusNotFound, "NOT_FOUND"},
		{"guest token on an authenticated job gets 403", authedJob, jobID, rawToken, "", http.StatusForbidden, "GUEST_TOKEN_INVALID"},
		{"unknown job gets 404", nil, uuid.New().String(), rawToken, "", http.StatusNotFound, "NOT_FOUND"},
		{"malformed job id gets 404", nil, "not-a-uuid", rawToken, "", http.StatusNotFound, "NOT_FOUND"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			ts := newStreamNode(t, mr, &key.PublicKey, streamQueries(tc.job))

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			resp := openStream(t, ctx, ts.URL, tc.jobID, tc.token, tc.bearer)

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantCode != "" {
				var body struct {
					Code string `json:"code"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
					t.Fatalf("decode error body: %v", err)
				}
				if body.Code != tc.wantCode {
					t.Fatalf("error code = %q, want %q", body.Code, tc.wantCode)
				}
				return
			}

			// Accepted: SSE headers + the snapshot event, then a clean
			// close (status is already terminal).
			if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
				t.Fatalf("Content-Type = %q, want text/event-stream", ct)
			}
			br := bufio.NewReader(resp.Body)
			want := `{"job_id":"` + tc.jobID + `","status":"delivered"}`
			if got := readSSEData(t, br); got != want {
				t.Fatalf("snapshot event = %s, want %s", got, want)
			}
			expectStreamClosed(t, br)
		})
	}
}

// TestStreamJob_RelayAddsJobIDBack pins the wire format: the internal
// job:<id>:status payload published by the hub deliberately omits job_id
// (the channel name scopes it -- link.statusPayload), and the handler
// must add it back so the client sees exactly
// `data: {"job_id":"uuid","status":"..."}` per plans/09-api-contracts.md.
// Also covers the terminal-close rule ('delivered' ends the stream).
func TestStreamJob_RelayAddsJobIDBack(t *testing.T) {
	key := newJWTKey(t)
	mr := miniredis.RunT(t)
	jobID := uuid.New().String()
	const rawToken = "relay-test-token"

	job := &fakeStreamJob{ID: jobID, SenderID: nil, GuestTokenHash: hashGuestTokenForTest(rawToken), Status: "printing"}
	ts := newStreamNode(t, mr, &key.PublicKey, streamQueries(job))

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp := openStream(t, ctx, ts.URL, jobID, rawToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)

	// Snapshot event first. Receiving it proves the handler's Redis
	// subscription is already active (it subscribes and waits for the
	// ack before writing the snapshot), so publishing after this point
	// cannot race the subscription.
	if got, want := readSSEData(t, br), `{"job_id":"`+jobID+`","status":"printing"}`; got != want {
		t.Fatalf("snapshot event = %s, want %s", got, want)
	}

	// Publish exactly what link.Hub.onStatus publishes: status only, no
	// job_id (link.jsonStatusPayload).
	receivers, err := rdb.Publish(ctx, "job:"+jobID+":status", `{"status":"delivered"}`).Result()
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if receivers == 0 {
		t.Fatal("expected the stream handler to be subscribed to job:<id>:status")
	}

	if got, want := readSSEData(t, br), `{"job_id":"`+jobID+`","status":"delivered"}`; got != want {
		t.Fatalf("relayed event = %s, want %s", got, want)
	}
	expectStreamClosed(t, br) // delivered is terminal
}

// TestStreamJob_FailedStatusClosesWithError covers the other terminal
// status: a failed payload (which carries the printer's error text in the
// internal payload) is relayed with job_id added and ends the stream.
func TestStreamJob_FailedStatusClosesWithError(t *testing.T) {
	key := newJWTKey(t)
	mr := miniredis.RunT(t)
	jobID := uuid.New().String()
	const rawToken = "failed-test-token"

	job := &fakeStreamJob{ID: jobID, SenderID: nil, GuestTokenHash: hashGuestTokenForTest(rawToken), Status: "dispatching"}
	ts := newStreamNode(t, mr, &key.PublicKey, streamQueries(job))

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp := openStream(t, ctx, ts.URL, jobID, rawToken, "")
	br := bufio.NewReader(resp.Body)
	if got, want := readSSEData(t, br), `{"job_id":"`+jobID+`","status":"dispatching"}`; got != want {
		t.Fatalf("snapshot event = %s, want %s", got, want)
	}

	if _, err := rdb.Publish(ctx, "job:"+jobID+":status", `{"status":"failed","error":"paper jam"}`).Result(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	want := `{"job_id":"` + jobID + `","status":"failed","error":"paper jam"}`
	if got := readSSEData(t, br); got != want {
		t.Fatalf("relayed event = %s, want %s", got, want)
	}
	expectStreamClosed(t, br) // failed is terminal
}

// TestStreamJob_CrossNodeStatusFanout is the Phase 5 roadmap's status
// fan-out check with two cloud "nodes" sharing one Redis: the printer's
// WebSocket is held by node A's hub, the browser's SSE connection by node
// B's handler. A status frame arriving up node A's printer link must
// reach the SSE client on node B -- the job:<id>:status pub/sub channel
// is the only bridge between them (plans/05-cloud-server.md "Works across
// nodes because all nodes publish to the same Redis"; mirror image of the
// dispatch fan-in test in link/hub_integration_test.go).
func TestStreamJob_CrossNodeStatusFanout(t *testing.T) {
	key := newJWTKey(t)
	mr := miniredis.RunT(t)
	jobID := uuid.New().String()
	mailboxID := uuid.New().String()
	const rawToken = "cross-node-token"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// --- Node A: accepts the printer's dial-out link. Its fake DB serves
	// the hub's UpdateJobStatus + InsertAuditEvent.
	sqlA := sql.OpenDB(fakeConnector{q: hubQueries(jobID, mailboxID)})
	defer sqlA.Close()
	rdbA := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdbA.Close()
	hubA := link.NewHub(rdbA, db.New(sqlA))

	nodeA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := hubA.Accept(r.Context(), w, r); err != nil {
			t.Logf("node A hub.Accept returned: %v", err)
		}
	}))
	defer nodeA.Close()

	printer, _, err := websocket.Dial(ctx, "ws"+nodeA.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("printer dial node A: %v", err)
	}
	defer printer.CloseNow()
	if err := wsjson.Write(ctx, printer, link.Frame{
		Type:          "register",
		MailboxID:     mailboxID,
		SlotOccupancy: map[string]link.SlotState{"slot-1": {Current: 0, Max: 5}},
	}); err != nil {
		t.Fatalf("printer register: %v", err)
	}

	// --- Node B: holds the sender's SSE connection. Its fake DB serves
	// GetJobForStream for the auth check + snapshot.
	job := &fakeStreamJob{ID: jobID, SenderID: nil, GuestTokenHash: hashGuestTokenForTest(rawToken), Status: "printing"}
	nodeB := newStreamNode(t, mr, &key.PublicKey, streamQueries(job))

	resp := openStream(t, ctx, nodeB.URL, jobID, rawToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE connect on node B: status = %d, want 200", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)

	// Snapshot proves node B's subscription is live before the printer
	// reports anything -- no publish/subscribe race below.
	if got, want := readSSEData(t, br), `{"job_id":"`+jobID+`","status":"printing"}`; got != want {
		t.Fatalf("snapshot event = %s, want %s", got, want)
	}

	// The printer (connected to node A) reports delivery.
	if err := wsjson.Write(ctx, printer, link.Frame{Type: "status", JobID: jobID, Status: "delivered"}); err != nil {
		t.Fatalf("printer status frame: %v", err)
	}

	// ...and the browser on node B sees it, job_id restored, then close.
	if got, want := readSSEData(t, br), `{"job_id":"`+jobID+`","status":"delivered"}`; got != want {
		t.Fatalf("cross-node event = %s, want %s", got, want)
	}
	expectStreamClosed(t, br)
}
