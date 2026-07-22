//go:build chaos

// Resilience & chaos (testing-plan Part 7 / Goal T9). One scripted driver kills
// each moving part of the assembled two-node stack in turn -- Redis, Postgres,
// the socket-owning cloud node, and the printer -- and proves the two properties
// the Verify line names:
//
//   - every job still reaches a terminal state EXACTLY ONCE. "Exactly once" is
//     read from the append-only audit_events table (the immutable ledger the T5
//     integration suite proved can't be UPDATE'd or DELETE'd): a job that was
//     double-printed would carry two `job_delivered` rows; a vanished job would
//     carry zero and never leave 'queued'. Counting those rows is a far stronger
//     assertion than eyeballing a status.
//   - the system RECONNECTS rather than crashing: after each kill, a fresh job
//     flows through (proving the cloud re-established its Redis/Postgres pools),
//     the printer's dial loop logs backoff-and-reconnect (not a panic), and no
//     service log carries a Go runtime crash.
//
// Ordering is deliberate. The dev printer reports a single slot with Max=5 that
// only ever increments in-process (print.go newSlotState) and resets solely when
// the printer PROCESS restarts. So the three scenarios that leave the printer
// running are budgeted to <5 total deliveries (redis 1, postgres 1, owner-kill
// 2 = 4), and the printer-restart scenario -- which resets occupancy to 0 -- runs
// last.
//
// Primitives (encrypt, submit, SSE stream, docker control, /dev/shm) live in
// harness.go, shared with the Goal T8 full-system driver. Brought up by
// scripts/e2e/chaos.sh; excluded from every default `go test ./...`.
package e2e

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

const streamTimeout = 120 * time.Second

// mailboxID is the seeded dev mailbox (docker-compose.yml DEV_MAILBOX_ID default).
const mailboxID = "00000000-0000-0000-0000-000000000001"

func TestChaos(t *testing.T) {
	ownerURL := env(t, "E2E_OWNER_URL")       // cloud-server: holds the printer socket
	nonOwnerURL := env(t, "E2E_NONOWNER_URL") // cloud-server-2: survivor node

	// Sanity: the stack is up and the printer is live before we start breaking
	// things (chaos.sh already gated on this, but fail loud if not).
	waitHealthz(t, ownerURL, 15*time.Second)
	waitHealthz(t, nonOwnerURL, 15*time.Second)
	waitPrinterIdle(t, 30*time.Second)

	// --- 1. Redis bounce: restart Redis under a live, idle printer. Redis holds
	// the routing/liveness cache, the pub/sub fabric, and the jobs:pending
	// stream+group; its data volume + RDB-on-SIGTERM survive a `restart`, and the
	// cloud's go-redis pools + pub/sub subscriptions must auto-reconnect. Proof of
	// recovery: a job submitted AFTER the bounce reaches delivered exactly once.
	t.Run("redis_bounce", func(t *testing.T) {
		dockerCompose(t, "restart", "redis")
		waitHealthz(t, ownerURL, 60*time.Second) // 200 only once its Redis PING succeeds again
		waitPrinterIdle(t, 60*time.Second)       // keepalive re-seeds the state cache
		time.Sleep(3 * time.Second)              // let pub/sub subscriptions re-establish
		runJobToDelivered(t, ownerURL, "redis-bounce")
		assertNoCrash(t, "cloud-server")
	})

	// --- 2. Postgres bounce: restart the durable store (jobs, audit ledger). Its
	// pg_data volume persists; database/sql reconnects transparently on the next
	// query. Proof: a fresh job round-trips to delivered exactly once afterward.
	t.Run("postgres_bounce", func(t *testing.T) {
		dockerCompose(t, "restart", "postgres")
		waitHealthz(t, ownerURL, 60*time.Second) // 200 only once its Postgres ping succeeds again
		waitPrinterIdle(t, 30*time.Second)
		runJobToDelivered(t, ownerURL, "postgres-bounce")
		assertNoCrash(t, "cloud-server")
	})

	// --- 3. Owner-node failover: stop the cloud node that holds the printer's
	// dial-out socket. The printer's socket dies and it enters its backoff dial
	// loop; meanwhile the SURVIVOR (cloud-server-2) must keep accepting work. Jobs
	// submitted during the outage land in jobs:pending as "queued" (no live socket
	// anywhere -> receivers==0 -> reverted+enqueued), i.e. nothing is lost. When
	// the owner restarts, the printer re-homes on it, becomes idle, and the queued
	// backlog drains -- each job delivered exactly once. The printer log shows the
	// backoff-reconnect (not a crash).
	t.Run("owner_node_failover", func(t *testing.T) {
		dockerCompose(t, "stop", "cloud-server")
		waitHealthz(t, nonOwnerURL, 20*time.Second) // survivor unaffected

		// Submit to the survivor during the outage; nothing can dispatch (no live
		// socket), so both must enqueue rather than vanish.
		var jobs []createJobResp
		for i := 0; i < 2; i++ {
			job := submitEncryptedJob(t, nonOwnerURL)
			if job.Status != "queued" {
				t.Fatalf("during owner outage, job %s got status %q, want \"queued\" "+
					"(no live socket -> must enqueue, not dispatch onto a dead node)", job.JobID, job.Status)
			}
			jobs = append(jobs, job)
		}
		if depth := redisStreamLen(t); depth < len(jobs) {
			t.Fatalf("jobs:pending depth %d < %d submitted during outage -- a queued job was lost", depth, len(jobs))
		}
		t.Logf("survivor accepted %d jobs into jobs:pending during the owner outage (no loss)", len(jobs))

		// Bring the owner back; the printer re-homes and the backlog drains.
		dockerCompose(t, "start", "cloud-server")
		waitHealthz(t, ownerURL, 60*time.Second)
		waitPrinterIdle(t, 90*time.Second)

		for _, job := range jobs {
			trail := streamToTerminal(t, nonOwnerURL, job.JobID, job.GuestToken, streamTimeout)
			assertDeliveredOnce(t, job.JobID, trail)
		}
		// The printer never restarted -- it reconnected -- so its dial loop must
		// show backoff-and-retry, and no service may have crashed.
		assertLogContains(t, "printer", "reconnecting in")
		assertNoCrash(t, "printer")
		assertNoCrash(t, "cloud-server")
		assertNoCrash(t, "cloud-server-2")
	})

	// --- 4. Printer crash + backpressure: kill the printer, pile up N jobs while
	// it is offline (all must queue), then restart it and confirm the whole
	// backlog drains EXACTLY ONCE with /dev/shm left clean. Runs last because the
	// printer restart resets its slot occupancy.
	t.Run("printer_crash_backpressure", func(t *testing.T) {
		const n = 3
		dockerCompose(t, "kill", "printer")
		waitPrinterExited(t, 30*time.Second)
		time.Sleep(4 * time.Second) // let the hub notice the closed socket + drop the subscriber

		var jobs []createJobResp
		for i := 0; i < n; i++ {
			job := submitEncryptedJob(t, ownerURL)
			if job.Status != "queued" {
				t.Fatalf("with the printer offline, job %s got status %q, want \"queued\"", job.JobID, job.Status)
			}
			jobs = append(jobs, job)
		}
		if depth := redisStreamLen(t); depth < n {
			t.Fatalf("backpressure: jobs:pending depth %d < %d -- a job was dropped while the printer was offline", depth, n)
		}
		t.Logf("backpressure OK: %d jobs parked in jobs:pending while the printer was down", n)

		dockerCompose(t, "start", "printer")
		waitPrinterIdle(t, 90*time.Second)

		for _, job := range jobs {
			trail := streamToTerminal(t, ownerURL, job.JobID, job.GuestToken, streamTimeout)
			assertDeliveredOnce(t, job.JobID, trail)
		}
		// A restarted printer registers fresh (recovery, not a wedge), the backlog
		// drained exactly once above, and the RAM-only invariant still holds.
		assertLogContains(t, "printer", "registered mailbox")
		assertNoCrash(t, "printer")
		assertDevShmClean(t)
	})
}

// runJobToDelivered submits one job on baseURL and requires it to reach
// "delivered" exactly once. Used by the dependency-bounce scenarios, where the
// only claim under test is "the cloud reconnected and the job completed" -- the
// intermediate status (dispatching vs a brief queue) is immaterial.
func runJobToDelivered(t *testing.T, baseURL, label string) {
	t.Helper()
	job := submitEncryptedJob(t, baseURL)
	trail := streamToTerminal(t, baseURL, job.JobID, job.GuestToken, streamTimeout)
	assertDeliveredOnce(t, job.JobID, trail)
	t.Logf("%s OK: job %s recovered to delivered (trail %v)", label, job.JobID, trail)
}

// assertDeliveredOnce is the exactly-once guard. The SSE trail must end in
// "delivered", and the immutable audit ledger must hold precisely one
// job_delivered row for the job -- zero means it vanished, two means it was
// double-printed. The job row must also read 'delivered'.
func assertDeliveredOnce(t *testing.T, jobID string, trail []string) {
	t.Helper()
	if len(trail) == 0 || trail[len(trail)-1] != "delivered" {
		t.Fatalf("job %s did not reach delivered (trail %v)", jobID, trail)
	}
	if n := countAuditAction(t, jobID, "job_delivered"); n != 1 {
		t.Fatalf("job %s has %d job_delivered audit rows, want exactly 1 (0=lost, >1=double-printed)", jobID, n)
	}
	if st := jobStatus(t, jobID); st != "delivered" {
		t.Fatalf("job %s row status is %q, want \"delivered\"", jobID, st)
	}
}

// --- infrastructure control + observation helpers ---------------------------

func waitHealthz(t *testing.T, baseURL string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz") //nolint:gosec // test-controlled localhost URL
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s/healthz never returned 200 within %s", baseURL, timeout)
}

// waitPrinterIdle polls the Redis liveness cache until the mailbox reports idle,
// i.e. the printer has (re)registered and the cloud can dispatch to it.
func waitPrinterIdle(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(redisCLI(t, "GET", "mailbox:"+mailboxID+":state"), "idle") {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("printer never reported idle within %s (mailbox:%s:state)", timeout, mailboxID)
}

// waitPrinterExited polls docker until the printer container is no longer
// running, so a subsequent submit can't race a still-live socket.
func waitPrinterExited(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		running := strings.Fields(dockerCompose(t, "ps", "--status", "running", "--services"))
		if !contains(running, "printer") {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("printer container still running %s after kill", timeout)
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func redisCLI(t *testing.T, args ...string) string {
	t.Helper()
	return strings.TrimSpace(dockerCompose(t, append([]string{"exec", "-T", "redis", "redis-cli"}, args...)...))
}

func redisStreamLen(t *testing.T) int {
	t.Helper()
	n, err := strconv.Atoi(redisCLI(t, "XLEN", "jobs:pending"))
	if err != nil {
		t.Fatalf("XLEN jobs:pending: %v", err)
	}
	return n
}

// psql runs a single query in the postgres container and returns the trimmed
// scalar result (-tAc). Credentials come from the env chaos.sh exports from .env.
func psql(t *testing.T, query string) string {
	t.Helper()
	out := dockerCompose(t, "exec", "-T",
		"-e", "PGPASSWORD="+env(t, "E2E_PG_PASSWORD"),
		"postgres", "psql",
		"-U", env(t, "E2E_PG_USER"), "-d", env(t, "E2E_PG_DB"),
		"-tAc", query)
	return strings.TrimSpace(out)
}

func countAuditAction(t *testing.T, jobID, action string) int {
	t.Helper()
	// jobID/action are fixed test constants + server-assigned UUIDs, not user input.
	n, err := strconv.Atoi(psql(t, "SELECT count(*) FROM audit_events WHERE job_id='"+jobID+"' AND action='"+action+"'"))
	if err != nil {
		t.Fatalf("count %s audit rows for job %s: %v", action, jobID, err)
	}
	return n
}

func jobStatus(t *testing.T, jobID string) string {
	t.Helper()
	return psql(t, "SELECT status FROM jobs WHERE id='"+jobID+"'")
}

// assertNoCrash fails if a service's logs carry a Go runtime crash. Ordinary
// reconnect noise (a logged Redis/Postgres error during a bounce) is expected
// resilience, not a crash, so only panic/fatal-error markers are flagged.
func assertNoCrash(t *testing.T, service string) {
	t.Helper()
	logs := dockerCompose(t, "logs", "--no-color", service)
	for _, marker := range []string{"panic:", "fatal error:", "goroutine stack exceeds"} {
		if strings.Contains(logs, marker) {
			t.Fatalf("%s crashed: log contains %q", service, marker)
		}
	}
}

func assertLogContains(t *testing.T, service, substr string) {
	t.Helper()
	logs := dockerCompose(t, "logs", "--no-color", service)
	if !strings.Contains(logs, substr) {
		t.Fatalf("%s log does not contain %q (expected reconnect/recovery evidence)", service, substr)
	}
}
