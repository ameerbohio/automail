package main

// Graceful-shutdown tests (plans/16-kubernetes.md §4.1, §4.4). Compose's
// forgiving lifecycle hid all of this: `docker compose down` SIGTERMs a
// process with no signal handling and nobody notices. Kubernetes SIGTERMs a
// pod on every rolling update, eviction and HPA scale-down, while it is often
// still receiving traffic -- so severing in-flight work is a user-visible bug,
// not untidiness.

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"automail/cloud/db"
	"automail/cloud/handlers"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// TestRunShutdown_RunsEveryStepInOrder pins the sequence itself. Order is not
// cosmetic: the consumer may only be deleted after the dispatcher loop has
// stopped, and the listeners must not be torn down before the drain signal has
// released the handlers parked in them.
func TestRunShutdown_RunsEveryStepInOrder(t *testing.T) {
	var ran []string
	steps := []shutdownStep{
		{name: "first", run: func(context.Context) error { ran = append(ran, "first"); return nil }},
		{name: "second", run: func(context.Context) error { ran = append(ran, "second"); return nil }},
		{name: "third", run: func(context.Context) error { ran = append(ran, "third"); return nil }},
	}

	runShutdown(context.Background(), steps)

	if got := strings.Join(ran, ","); got != "first,second,third" {
		t.Fatalf("steps ran %q, want first,second,third", got)
	}
}

// TestRunShutdown_ContinuesPastAFailingStep is the deliberate policy: a failed
// step must not abort the rest. The later steps (draining HTTP, closing the
// internal listener) matter most precisely when something has already gone
// wrong, and every step is independently safe to attempt.
func TestRunShutdown_ContinuesPastAFailingStep(t *testing.T) {
	var ran []string
	runShutdown(context.Background(), []shutdownStep{
		{name: "ok", run: func(context.Context) error { ran = append(ran, "ok"); return nil }},
		{name: "boom", run: func(context.Context) error { ran = append(ran, "boom"); return errors.New("nope") }},
		{name: "after", run: func(context.Context) error { ran = append(ran, "after"); return nil }},
	})

	if got := strings.Join(ran, ","); got != "ok,boom,after" {
		t.Fatalf("steps ran %q, want all three (a failure must not abort the sequence)", got)
	}
}

// TestRunShutdown_SharesOneDeadline: the grace period is a whole-sequence
// budget, not a per-step one. A wedged step burns the budget, and every step
// after it sees an already-expired context and returns at once rather than
// each waiting out its own timeout past the SIGKILL.
func TestRunShutdown_SharesOneDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	var lateStepDeadlineExceeded bool
	runShutdown(ctx, []shutdownStep{
		{name: "slow", run: func(ctx context.Context) error {
			<-ctx.Done() // consumes the whole budget
			return ctx.Err()
		}},
		{name: "late", run: func(ctx context.Context) error {
			lateStepDeadlineExceeded = errors.Is(ctx.Err(), context.DeadlineExceeded)
			return nil
		}},
	})

	if !lateStepDeadlineExceeded {
		t.Fatal("the step after a budget-consuming one should see an expired context, not a fresh timeout")
	}
}

func TestWaitFor(t *testing.T) {
	t.Run("returns when done closes", func(t *testing.T) {
		done := make(chan struct{})
		close(done)
		if err := waitFor(context.Background(), done); err != nil {
			t.Fatalf("waitFor = %v, want nil", err)
		}
	})

	t.Run("gives up at the deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if err := waitFor(ctx, make(chan struct{})); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waitFor on a never-closing channel = %v, want DeadlineExceeded", err)
		}
	})
}

// newDrainableStreamNode is newStreamNode (stream_test.go) with a drain
// channel wired in, matching how main.go builds the Server.
func newDrainableStreamNode(t *testing.T, mr *miniredis.Miniredis, q fakeQueryFunc, drain <-chan struct{}) *httptest.Server {
	t.Helper()
	sqlDB := sql.OpenDB(fakeConnector{q: q})
	t.Cleanup(func() { sqlDB.Close() })
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	srv := &handlers.Server{Queries: db.New(sqlDB), SQLDB: sqlDB, Redis: rdb, Drain: drain}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", srv.Healthz)
	mux.HandleFunc("GET /livez", srv.Livez)
	mux.Handle("GET /jobs/{id}/stream", optionalAuth(nil)(http.HandlerFunc(srv.StreamJob)))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// TestStreamJob_DrainClosesStreamDeliberately is the §4.1 fix in one test.
//
// http.Server.Shutdown waits for in-flight requests but never cancels their
// contexts, so StreamJob's `<-r.Context().Done()` does NOT fire on shutdown:
// every open SSE stream would pin the drain for the entire grace period and
// then be severed by process exit anyway. The drain channel is the explicit
// signal that lets the handler end the stream itself -- and the SSE comment it
// writes on the way out is what distinguishes "the server closed this" from
// "the process was killed" on the wire.
func TestStreamJob_DrainClosesStreamDeliberately(t *testing.T) {
	mr := miniredis.RunT(t)
	jobID := uuid.New().String()
	const rawToken = "drain-test-token"

	job := &fakeStreamJob{ID: jobID, SenderID: nil, GuestTokenHash: hashGuestTokenForTest(rawToken), Status: "printing"}
	drain := make(chan struct{})
	ts := newDrainableStreamNode(t, mr, streamQueries(job), drain)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp := openStream(t, ctx, ts.URL, jobID, rawToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)

	// The snapshot event proves the handler is parked in its select loop --
	// i.e. this is a genuinely in-flight, long-lived request, the case
	// Shutdown alone cannot release.
	if got, want := readSSEData(t, br), `{"job_id":"`+jobID+`","status":"printing"}`; got != want {
		t.Fatalf("snapshot event = %s, want %s", got, want)
	}

	start := time.Now()
	close(drain)

	// A comment line ends the stream: a deliberate close, not a severed conn.
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the drain notice: %v", err)
	}
	if !strings.HasPrefix(line, ": draining") {
		t.Fatalf("got %q, want a `: draining` SSE comment before the close", line)
	}
	if sep, err := br.ReadString('\n'); err != nil || sep != "\n" {
		t.Fatalf("comment separator = %q (err=%v), want a blank line", sep, err)
	}
	expectStreamClosed(t, br)

	// "Deliberately rather than on timeout" is the acceptance wording; a
	// handler that only exited when the grace period expired would fail here.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("stream took %s to end after the drain signal; it must end promptly", elapsed)
	}
}

// TestProbes_ReadinessVsLiveness pins the §4.4 split, whose whole value is in
// the two endpoints disagreeing while draining: readiness must fail so nothing
// new is routed here, and liveness must NOT, or the kubelet restarts a process
// that is shutting down correctly.
func TestProbes_ReadinessVsLiveness(t *testing.T) {
	mr := miniredis.RunT(t)
	drain := make(chan struct{})
	ts := newDrainableStreamNode(t, mr, nopQuery, drain)

	get := func(path string) int {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if got := get("/healthz"); got != http.StatusOK {
		t.Fatalf("readiness before drain = %d, want 200", got)
	}
	if got := get("/livez"); got != http.StatusOK {
		t.Fatalf("liveness before drain = %d, want 200", got)
	}

	close(drain)

	if got := get("/healthz"); got != http.StatusServiceUnavailable {
		t.Fatalf("readiness while draining = %d, want 503 (traffic must stop being routed here)", got)
	}
	if got := get("/livez"); got != http.StatusOK {
		t.Fatalf("liveness while draining = %d, want 200 (a draining process is alive; failing this gets it SIGKILLed mid-drain)", got)
	}
}

// TestLivez_TouchesNoDependencies is the other half of §4.4: liveness must
// answer "is this process wedged", never "is the world healthy". If it checked
// Postgres or Redis, a dependency blip would restart every cloud-server pod at
// once -- turning a wobble into a total outage. Both dependencies are dead
// here and it must still answer 200.
func TestLivez_TouchesNoDependencies(t *testing.T) {
	mr := miniredis.RunT(t)
	sqlDB := sql.OpenDB(fakeConnector{q: nopQuery})
	sqlDB.Close() // Postgres gone
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	mr.Close() // Redis gone

	srv := &handlers.Server{Queries: db.New(sqlDB), SQLDB: sqlDB, Redis: rdb}

	w := httptest.NewRecorder()
	srv.Livez(w, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("liveness with both dependencies down = %d, want 200", w.Code)
	}

	w = httptest.NewRecorder()
	srv.Healthz(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness with both dependencies down = %d, want 503 (the probes must differ)", w.Code)
	}
}
