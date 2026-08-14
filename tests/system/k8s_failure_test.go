//go:build k8sfail

// Cluster failure and rollout behaviour (Goal K6, plans/16-kubernetes.md §5,
// §8) -- the three things Compose cannot express, driven against the real k3d
// cluster with the printer outside it.
//
// WHY THIS IS NOT A REPEAT OF THE COMPOSE CHAOS SUITE (Goal T9). T9 recorded an
// honest boundary: the Compose printer dials a fixed service alias, so when the
// node holding its socket goes away there is nowhere for the socket to fail
// *over* to -- the survivor could only buffer work, and the XAUTOCLAIM reclaim
// path was cited from the T5 Redis integration test rather than demonstrated in
// the assembled product. Behind a NodePort Service the printer's redial lands on
// whichever pod kube-proxy picks, so both properties become measurable here:
//
//  1. socket_owner_deleted   -- the socket moves to a DIFFERENT pod, and work
//     submitted while no socket existed anywhere is
//     not lost.
//  2. pel_holder_deleted     -- a job already held in a pod's Pending Entries
//     List when that pod dies is recovered by
//     XAUTOCLAIM on a survivor. This is the real
//     crash-recovery path, exercised end-to-end for
//     the first time in the project.
//  3. pdb_refuses_eviction / node_drain
//     -- voluntary disruption against a
//     PodDisruptionBudget: one eviction allowed, the
//     second refused, and a full node drain.
//  4. rolling_update         -- `kubectl rollout restart` under continuous
//     traffic with zero non-2xx.
//
// TWO DELIBERATE INSTRUMENTS, called out because they are the difference
// between a measurement and a coincidence:
//
//   - `docker pause` on the printer, in the socket-failover scenario only. The
//     printer's reconnect backoff is ~1s, so the window in which no cloud pod
//     holds its socket is about one second wide. Racing it with HTTP
//     submissions would make "the job was queued, not dispatched" a coin flip.
//     Pausing the container freezes it mid-connection: the outage window
//     becomes as long as the test needs, and the unpause is still a genuine
//     reconnect -- the process wakes to find its TCP peer gone and redials,
//     exactly as it would after a real network partition. Note what a pause
//     does NOT do: it leaves the TCP connection open, so the cloud pod keeps
//     the socket and stays subscribed. The reclaim scenario therefore stops the
//     container outright -- see its comment.
//   - pre-uploaded jobs. Everything except the final `POST /jobs` (recipient
//     lookup, key fetch, encrypt, pre-signed PUT) happens before the pod is
//     killed, so what lands inside the outage window is one small request whose
//     returned status is the assertion.
//
// Run with `make k8s-failure`, never bare `go test`: scripts/k8s/failure-check.sh
// seeds the cluster, starts the printer outside it, resolves the socket owner
// and sets every env knob below.
package system

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The Redis objects the dispatch path is built on (services/cloud/store,
// services/cloud/dispatch). Named here so the assertions quote one source.
const (
	pendingStream = "jobs:pending"
	consumerGroup = "dispatchers"
)

// claimMinIdleWindow mirrors dispatch.claimMinIdle (60s) plus its sweep ticker,
// which runs on the same period: an entry orphaned in a dead consumer's PEL
// becomes claimable after 60s, and the next survivor sweep can be up to another
// 60s away. 240s leaves headroom over that worst case without being so generous
// that a genuinely stuck job looks like a slow one.
const claimMinIdleWindow = 240 * time.Second

// results accumulates the measured facts the run writes into
// infra/k8s/RESULTS.md. Goal K8 has to trace its resume numbers back to
// something committed, which is why this is a file in the tree rather than
// scrollback.
var (
	resultsMu    sync.Mutex
	results      []string
	deletedPods  []string // pods this run removed on purpose; consumer-group bookkeeping expects them
	mailboxIDVar string
)

func record(format string, args ...any) {
	resultsMu.Lock()
	defer resultsMu.Unlock()
	results = append(results, fmt.Sprintf(format, args...))
}

func TestMain(m *testing.M) {
	root := os.Getenv("E2E_REPO_ROOT")
	if root == "" {
		fmt.Fprintln(os.Stderr, "E2E_REPO_ROOT is unset -- run via `make k8s-failure`, not bare `go test`")
		os.Exit(1)
	}
	port := os.Getenv("K8S_EDGE_HTTPS_PORT")
	if port == "" {
		port = "9443"
	}
	if err := installEdgeTransport(root, "127.0.0.1", port); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	mailboxIDVar = os.Getenv("K8S_MAILBOX_ID")
	if mailboxIDVar == "" {
		mailboxIDVar = "00000000-0000-0000-0000-000000000001"
	}
	os.Exit(m.Run())
}

// TestK8sFailureAndRollout runs the scenarios in one parent test so their order
// is fixed (each leaves the cluster in the state the next assumes) and so the
// results file is written exactly once, from a single place, whatever happened.
func TestK8sFailureAndRollout(t *testing.T) {
	apiURL := "https://" + apiHost

	defer writeResults(t)

	record("- Run: `%s` against k3d cluster `%s`, k3s `%s`.",
		time.Now().Format(time.RFC3339), os.Getenv("K8S_NAMESPACE"), nodeKubeletVersion(t))

	t.Run("socket_owner_deleted", func(t *testing.T) { scenarioSocketOwnerDeleted(t, apiURL) })
	t.Run("pel_holder_deleted", func(t *testing.T) { scenarioPELHolderDeleted(t, apiURL) })
	t.Run("pdb_and_node_drain", func(t *testing.T) { scenarioDrainAgainstPDB(t) })
	t.Run("rolling_update", func(t *testing.T) { scenarioRollingUpdate(t, apiURL) })
}

// --- scenario 1: the pod holding the printer's socket is deleted ------------

// scenarioSocketOwnerDeleted proves the property Compose structurally cannot:
// the printer's socket FAILS OVER. The pod kube-proxy first handed the dial to
// is deleted; the printer redials the same NodePort and is answered by a
// different pod. Work submitted while no pod anywhere held the socket is
// accepted, parked in the Stream, and delivered exactly once afterwards.
func scenarioSocketOwnerDeleted(t *testing.T, apiURL string) {
	owner := env(t, "K8S_OWNER_POD")

	if n := dispatchSubscribers(t); n != 1 {
		t.Fatalf("expected exactly 1 subscriber on %s before the kill, got %d "+
			"(0 = the printer is not registered; >1 = a stale Compose printer is also connected)",
			chanDispatch(), n)
	}

	// Everything except the final POST happens now, so the outage window has to
	// absorb one ~20ms request instead of a four-call browser flow.
	prepared := []preparedJob{prepareJob(t, apiURL), prepareJob(t, apiURL)}
	t.Logf("pre-uploaded %d ciphertext blobs; only POST /jobs is left to fire", len(prepared))

	// Freeze the printer so the outage window is deterministic rather than a
	// race against its ~1s reconnect backoff (see the file header).
	dockerCmd(t, "pause", env(t, "E2E_PRINTER_CONTAINER"))
	unpaused := false
	defer func() {
		if !unpaused {
			_, _ = exec.Command("docker", "unpause", os.Getenv("E2E_PRINTER_CONTAINER")).CombinedOutput()
		}
	}()

	// --wait=false: the pod takes its full graceful shutdown (preStop 5s +
	// SHUTDOWN_TIMEOUT 20s), and what this test wants to observe is the moment
	// its socket disappears, not the moment the API server finishes the delete.
	start := time.Now()
	kubectlNS(t, "delete", "pod", owner, "--wait=false")
	deletedPods = append(deletedPods, owner)
	t.Logf("deleted socket owner %s", owner)

	waitDispatchSubscribers(t, 0, 90*time.Second)
	lostAfter := time.Since(start)
	t.Logf("no cloud pod holds the printer socket after %s", lostAfter.Round(100*time.Millisecond))
	record("- Socket owner `%s` deleted; the last subscriber on `%s` disappeared **%s** after "+
		"the delete (preStop 5s + graceful drain, so this is the shutdown budget being spent, not a hang).",
		owner, chanDispatch(), lostAfter.Round(100*time.Millisecond))

	// The window itself: with zero subscribers, dispatch must REFUSE to claim
	// the job (route.go reverts and returns blocked when Publish reports 0
	// receivers) and park it in the Stream instead.
	var jobs []createJobResp
	for i, p := range prepared {
		job := postPreparedJob(t, apiURL, p)
		if job.Status != "queued" {
			t.Fatalf("job %d submitted during the socket outage got status %q, want \"queued\" -- "+
				"a dispatch with zero live sockets must revert the claim and enqueue, never report progress",
				i, job.Status)
		}
		jobs = append(jobs, job)
	}
	depth := streamLen(t)
	if depth < len(jobs) {
		t.Fatalf("%s holds %d entries, fewer than the %d jobs just queued -- work was dropped",
			pendingStream, depth, len(jobs))
	}
	record("- %d jobs submitted through the ingress during that window were accepted as `queued` and "+
		"parked in `%s` (depth %d): with no live socket anywhere, `attemptDispatch` publishes, sees "+
		"0 receivers, reverts the Postgres claim and re-queues rather than reporting progress.",
		len(jobs), pendingStream, depth)

	// Unpause: the printer wakes to a dead TCP peer and redials the NodePort.
	dockerCmd(t, "unpause", env(t, "E2E_PRINTER_CONTAINER"))
	unpaused = true
	reconnect := time.Now()
	waitDispatchSubscribers(t, 1, 120*time.Second)
	t.Logf("printer re-registered %s after unpause", time.Since(reconnect).Round(100*time.Millisecond))

	newOwner := socketOwner(t, reconnect.UTC().Add(-2*time.Second).Format(time.RFC3339))
	if newOwner == owner {
		t.Fatalf("the socket came back on %s, the pod that was deleted -- impossible, so the log scan "+
			"is reading a stale line", owner)
	}
	record("- The printer redialled `wss://localhost:9843` and was answered by **`%s`** (was `%s`), "+
		"%s after the unpause. This is the property Compose cannot have: there the printer dials a "+
		"fixed service alias, so the socket can only come back on the same name.",
		newOwner, owner, time.Since(reconnect).Round(100*time.Millisecond))

	// The backlog drains: registration publishes mailbox:<id>:available, every
	// pod's dispatcher wakes and one of them wins the XREADGROUP.
	for _, job := range jobs {
		trail := streamToTerminal(t, apiURL, job.JobID, job.GuestToken, 120*time.Second)
		assertDeliveredExactlyOnce(t, job.JobID, trail)
	}
	assertDevShmClean(t)
	record("- Both queued jobs reached `delivered` after the failover, each with exactly one " +
		"`job_delivered` row in the append-only `audit_events` ledger (0 would mean lost, 2 would mean " +
		"double-printed), and the printer's `/dev/shm` was empty afterwards.")

	waitDeploymentReady(t, 180*time.Second)
	record("- The Deployment returned to 3/3 Ready on its own; the ReplicaSet replaced the deleted pod.")
}

// --- scenario 2: the pod holding the PEL entry is deleted -------------------

// scenarioPELHolderDeleted is the crash-recovery path, and the reason
// plans/03-scaling.md chose Redis Streams over plain pub/sub.
//
// A stream entry that a consumer has read but not ACK'd is in that consumer's
// Pending Entries List. It is invisible to every other consumer's XREADGROUP
// ">" -- Redis considers it delivered -- so if the consumer dies, only
// XAUTOCLAIM can recover it. That is the failure this test manufactures on
// purpose:
//
//	printer STOPPED ->  job submitted  ->  queued in the Stream
//	a pod's 60s sweep reads it, dispatch is still blocked, so it is NOT ACK'd
//	that pod is deleted            <- the entry is now orphaned in its PEL
//	printer restarted, registers   <- every survivor drains ">" and finds NOTHING
//	60s idle elapses, a survivor's sweep XAUTOCLAIMs it and dispatches
//
// It also proves the Goal K0 guard that makes this survivable: shutdown does
// NOT run XGROUP DELCONSUMER when the consumer still owns pending entries,
// because DELCONSUMER *discards* the PEL rather than handing it back.
//
// WHY THIS ONE STOPS THE PRINTER INSTEAD OF PAUSING IT, unlike the scenario
// above. Pausing froze the printer but left its TCP connection open, so the
// cloud pod kept the socket and stayed subscribed to `mailbox:<id>:dispatch` --
// dispatch went on succeeding into a process that could not answer. A paused
// peer is not a disconnected peer, and TCP offers no way to tell the
// difference; only the far end closing (or a keepalive timing out) does. The
// scenario above works with a pause because what removes the subscriber there
// is the POD being deleted, not the printer being frozen. Here nothing is
// deleted until later, so the socket has to be closed for real.
func scenarioPELHolderDeleted(t *testing.T, apiURL string) {
	printer := env(t, "E2E_PRINTER_CONTAINER")
	dockerCmd(t, "stop", printer)
	restarted := false
	defer func() {
		if !restarted {
			_, _ = exec.Command("docker", "start", printer).CombinedOutput()
		}
	}()
	waitDispatchSubscribers(t, 0, 60*time.Second)

	job := postPreparedJob(t, apiURL, prepareJob(t, apiURL))
	if job.Status != "queued" {
		t.Fatalf("with the printer stopped the job got status %q, want \"queued\"", job.Status)
	}
	t.Logf("job %s parked in %s", job.JobID, pendingStream)

	// Wait for some pod's sweep to read it. Nothing can dispatch it, so
	// handle() leaves it un-ACK'd in that pod's PEL -- which is what the next
	// step needs, and is itself the behaviour worth asserting (a retry that
	// re-XADDs would multiply the entry instead).
	holder, entryID, waited := waitForPELEntry(t, job.JobID, 150*time.Second)
	t.Logf("job entry sits un-ACK'd in %s's PEL after %s", holder, waited.Round(time.Second))
	record("- A job queued with no printer available was read by pod **`%s`** after %s and left "+
		"un-ACK'd in its PEL -- `handle()` leaves a still-blocked entry pending rather than "+
		"re-`XADD`ing it, so the entry is never multiplied.",
		holder, waited.Round(time.Second))

	kubectlNS(t, "delete", "pod", holder, "--wait=true", "--timeout=90s")
	deletedPods = append(deletedPods, holder)

	// The K0 guard: the dead pod's consumer must STILL be in the group, because
	// deleting it would have destroyed the PEL entry along with it.
	pending := consumerPending(t)
	if n, ok := pending[holder]; !ok || n == 0 {
		t.Fatalf("consumer %s is gone from group %s (or holds nothing) after its pod was deleted: %v\n"+
			"XGROUP DELCONSUMER discards the PEL, so shutdown must skip it while entries are pending "+
			"(services/cloud/dispatch/dispatcher.go RemoveConsumer) -- otherwise this job is lost",
			holder, consumerGroup, pending)
	}
	record("- Deleting `%s` left its consumer in the `%s` group holding %d pending entry: graceful "+
		"shutdown skips `XGROUP DELCONSUMER` while the PEL is non-empty, because DELCONSUMER discards "+
		"pending entries instead of returning them (the Goal K0 trap, guarded here for real).",
		holder, consumerGroup, pending[holder])

	dockerCmd(t, "start", printer)
	restarted = true
	waitDispatchSubscribers(t, 1, 120*time.Second)

	// The registration publishes mailbox:<id>:available and every survivor
	// drains ">" immediately -- and finds nothing, because the entry belongs to
	// a dead consumer's PEL. Only the 60s-idle XAUTOCLAIM sweep can recover it,
	// so this wait is the reclaim path being timed, not slack.
	claimStart := time.Now()
	waitJobStatus(t, job.JobID, "delivered", claimMinIdleWindow)
	reclaimed := time.Since(claimStart)

	if n := auditCount(t, job.JobID, "job_delivered"); n != 1 {
		t.Fatalf("job %s has %d job_delivered rows, want exactly 1", job.JobID, n)
	}
	assertDevShmClean(t)

	claimer, evidence := reclaimEvidence(t, entryID)
	if evidence == "" {
		t.Fatalf("job %s (stream entry %s) reached delivered, but no surviving pod logged a reclaim of "+
			"that entry -- if it recovered through the ordinary XREADGROUP path then the entry was "+
			"never orphaned and this scenario proved nothing", job.JobID, entryID)
	}
	t.Logf("reclaimed by %s: %s", claimer, evidence)
	record("- After the printer reconnected, the orphaned entry was invisible to every survivor's "+
		"`XREADGROUP >`; **`%s`** recovered it via `XAUTOCLAIM` %s later (log: `%s`) and the job "+
		"reached `delivered` with exactly one ledger row. The delay is `claimMinIdle` (60s) plus the "+
		"sweep period, not latency -- an entry must be *provably* abandoned before another node may "+
		"take it, or two nodes would print the same letter.",
		claimer, reclaimed.Round(time.Second), evidence)
}

// --- scenario 3: voluntary disruption against the PDB -----------------------

// scenarioDrainAgainstPDB exercises the difference between "I restarted things"
// and "I reasoned about voluntary disruption". The PDB is minAvailable: 2
// against replicas: 3, so exactly one pod may be disrupted at a time. Both
// halves of that are asserted: an eviction that is allowed, and a second one
// that the API server refuses with 429.
func scenarioDrainAgainstPDB(t *testing.T) {
	waitDeploymentReady(t, 180*time.Second)
	healthy, desired, allowed := pdbStatus(t)
	if allowed != 1 {
		t.Fatalf("PDB cloud-server allows %d disruptions (currentHealthy %d, desiredHealthy %d), want 1 -- "+
			"the scenario below assumes exactly one eviction fits in the budget", allowed, healthy, desired)
	}
	record("- `PodDisruptionBudget/cloud-server` before the drain: currentHealthy %d, desiredHealthy %d, "+
		"disruptionsAllowed %d.", healthy, desired, allowed)

	pods := cloudPods(t)
	if len(pods) < 3 {
		t.Fatalf("need 3 cloud-server pods, have %d", len(pods))
	}

	// First eviction: inside the budget, so it must succeed.
	if out, err := evictPod(t, pods[0]); err != nil {
		t.Fatalf("first eviction of %s was refused but the budget allowed one: %v\n%s", pods[0], err, out)
	}
	deletedPods = append(deletedPods, pods[0])

	// Second eviction: only meaningful once the budget has actually closed, so
	// wait for disruptionsAllowed to hit 0 rather than assuming it did. If the
	// replacement becomes Ready first the budget reopens and the refusal would
	// never happen -- that is a timing artefact, not a property, so it is
	// reported rather than asserted.
	if !waitPDBAllowed(t, 0, 30*time.Second) {
		record("- The second eviction was NOT attempted: the replacement pod became Ready before the " +
			"budget closed, so `disruptionsAllowed` never dropped to 0 in this run.")
	} else {
		out, err := evictPod(t, pods[1])
		if err == nil {
			t.Fatalf("evicting %s succeeded while disruptionsAllowed was 0 -- the PDB is not being "+
				"enforced:\n%s", pods[1], out)
		}
		if !strings.Contains(out, "disruption budget") {
			t.Fatalf("the second eviction failed for the wrong reason (want a disruption-budget "+
				"refusal):\n%s", out)
		}
		line := firstLine(out)
		t.Logf("second eviction refused: %s", line)
		record("- With one pod already disrupted, `disruptionsAllowed` fell to 0 and the API server "+
			"**refused** the second eviction: `%s`. Kubernetes declined to trade availability for "+
			"convenience; nothing in the application had to know.", line)
	}
	waitDeploymentReady(t, 180*time.Second)

	// The drain proper. Never the server node: the k3d-local overlay pins the
	// data tier there because local-path PVs carry a nodeAffinity to the node
	// they bound on, so draining it would strand Postgres in Pending forever.
	node := drainableNode(t)
	before := placement(t)
	t.Logf("draining %s (placement before: %s)", node, before)

	drainStart := time.Now()
	out, err := kubectlTry(t, "drain", node,
		"--ignore-daemonsets", "--delete-emptydir-data", "--timeout=180s")
	uncordoned := false
	defer func() {
		if !uncordoned {
			_, _ = kubectlTry(t, "uncordon", node)
		}
	}()
	if err != nil {
		t.Fatalf("kubectl drain %s failed: %v\n%s", node, err, out)
	}
	drained := time.Since(drainStart)

	if pods := podsOnNode(t, node); len(pods) > 0 {
		t.Fatalf("node %s still runs cloud-server pods after the drain: %v", node, pods)
	}
	waitDeploymentReady(t, 180*time.Second)
	after := placement(t)
	record("- `kubectl drain %s --ignore-daemonsets --delete-emptydir-data` completed in %s while the "+
		"PDB held. Pod placement went from `%s` to `%s`: the evicted replica rescheduled onto a "+
		"remaining node because the anti-affinity is **preferred**, not required -- a required rule "+
		"would have turned this drain into an outage. The node was chosen to be neither the server "+
		"node (the data tier's `local-path` PVs are pinned there) nor the node running Traefik "+
		"(draining that moves the ingress every other scenario measures through).",
		node, drained.Round(time.Second), before, after)

	kubectlOut(t, "uncordon", node)
	uncordoned = true
	record("- The node was uncordoned afterwards; nothing else was left cordoned.")
}

// --- scenario 4: rolling update under continuous traffic --------------------

// scenarioRollingUpdate is the claim `maxUnavailable: 0` + readiness + preStop
// exist to support: every pod in the tier is replaced, under load, without a
// single failed request. The traffic deliberately targets /healthz on
// api.automail.local -- the catch-all router, NOT the guest rate-limited one --
// because a 429 would read as a dropped request and prove the opposite of what
// is being measured.
func scenarioRollingUpdate(t *testing.T, apiURL string) {
	waitDeploymentReady(t, 180*time.Second)
	// The scenarios before this one moved pods around; start only once the edge
	// has been quiet for a while, or their churn is charged to this measurement.
	waitEdgeStable(t, apiURL+"/healthz", 20, 90*time.Second)
	oldPods := cloudPods(t)

	const workers = 4
	const pace = 100 * time.Millisecond
	var (
		total      atomic.Int64
		nonOK      atomic.Int64
		errs       atomic.Int64
		firstBad   atomic.Value // string
		nodesSeen  sync.Map
		stop       = make(chan struct{})
		wg         sync.WaitGroup
		trafficTop = time.Now()
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, err := httpGetNoBody(apiURL + "/healthz")
				total.Add(1)
				switch {
				case err != nil:
					errs.Add(1)
					firstBad.CompareAndSwap(nil, fmt.Sprintf("t+%s transport: %v",
						time.Since(trafficTop).Round(time.Millisecond), err))
				case resp.status/100 != 2:
					nonOK.Add(1)
					firstBad.CompareAndSwap(nil, fmt.Sprintf("t+%s HTTP %d from node %q",
						time.Since(trafficTop).Round(time.Millisecond), resp.status, resp.node))
				default:
					if resp.node != "" {
						nodesSeen.Store(resp.node, struct{}{})
					}
				}
				// Paced on EVERY path, failures included. An earlier version
				// `continue`d past this on error and turned a handful of real
				// failures into 94k hot-looped ones, which buried the first and
				// only interesting sample under six digits of consequence.
				time.Sleep(pace)
			}
		}()
	}

	// Let the baseline settle so a failure can't be blamed on a cold start.
	time.Sleep(3 * time.Second)
	rolloutStart := time.Now()
	kubectlNS(t, "rollout", "restart", "deploy/cloud-server")
	kubectlNS(t, "rollout", "status", "deploy/cloud-server", "--timeout=300s")
	rolloutTook := time.Since(rolloutStart)
	time.Sleep(2 * time.Second) // catch anything the last pod's teardown breaks
	close(stop)
	wg.Wait()

	var distinct []string
	nodesSeen.Range(func(k, _ any) bool { distinct = append(distinct, k.(string)); return true })

	if bad := errs.Load() + nonOK.Load(); bad != 0 {
		t.Fatalf("rolling update dropped %d of %d requests (%d non-2xx, %d transport errors); first: %v\n"+
			"maxUnavailable: 0 + a readiness probe + the 5s preStop sleep are exactly what is supposed "+
			"to prevent this -- a 502/503 here means traffic reached a pod that was already draining",
			bad, total.Load(), nonOK.Load(), errs.Load(), firstBad.Load())
	}
	newPods := cloudPods(t)
	if overlap := intersect(oldPods, newPods); len(overlap) > 0 {
		t.Fatalf("pods %v survived the rollout -- the Deployment did not actually replace the tier", overlap)
	}
	if len(distinct) < 2 {
		t.Fatalf("all %d requests were served by %v; the traffic never reached more than one pod, so "+
			"'zero dropped requests' would not mean much", total.Load(), distinct)
	}

	t.Logf("rollout took %s; %d requests, 0 failures, served by %d distinct pods",
		rolloutTook.Round(time.Second), total.Load(), len(distinct))
	record("- `kubectl rollout restart deploy/cloud-server` replaced all 3 pods in %s under continuous "+
		"traffic (%d requests at ~%d req/s through the Traefik ingress to `/healthz` on "+
		"`api.automail.local`): **0 non-2xx, 0 transport errors**, answered by %d distinct pods "+
		"(`X-Automail-Node`). The traffic uses the catch-all router deliberately -- the guest "+
		"rate-limited router would have returned 429s that read as dropped requests.",
		rolloutTook.Round(time.Second), total.Load(), workers*10, len(distinct))

	// The K0 regression guard, in the place it matters most: NODE_ID is the pod
	// name (downward API), so a rollout changes every consumer name. Without
	// DELCONSUMER on graceful stop this group would grow by three per rollout.
	//
	// THIS HAS TO CONVERGE RATHER THAN SAMPLE. `kubectl rollout status` returns
	// as soon as the new ReplicaSet is fully available, which is strictly
	// earlier than the old pods finishing: each still has up to its 35s grace
	// period to run preStop, drain, and only then DELCONSUMER itself. Reading
	// the group at that instant reports consumers for pods that are mid-exit and
	// calls them leaks. What makes a leak a leak is that it never goes away.
	stale := waitConsumersConverge(t, newPods, 90*time.Second)
	record("- After the rollout the `%s` group settled to one consumer per live pod (%d), the Goal K0 "+
		"`XGROUP DELCONSUMER` guard doing its job: `NODE_ID` is the pod name, so without it every "+
		"`rollout restart` would leak three consumers forever. Residual consumers: %v — pods this run "+
		"killed deliberately, left for the idle reaper (they are not leaks, and deleting one while it "+
		"held pending entries is precisely what must not happen). The check waits for convergence: "+
		"`rollout status` returns before the outgoing pods have spent their termination grace, and a "+
		"consumer that is still mid-exit is not a leak.",
		consumerGroup, len(newPods), stale)
}

// waitConsumersConverge blocks until the `dispatchers` group holds exactly one
// consumer per live pod, plus (allowably) the pods this run killed on purpose,
// and returns those residuals. A consumer for a pod that no longer exists and
// was never deliberately killed is the leak Goal K0 fixed.
func waitConsumersConverge(t *testing.T, livePodNames []string, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var problem string
	for {
		pending := consumerPending(t)
		problem = ""
		var stale []string
		for name := range pending {
			if containsStr(livePodNames, name) {
				continue
			}
			stale = append(stale, name)
			if !containsStr(deletedPods, name) {
				problem = fmt.Sprintf("consumer %s belongs to no live pod and was not one this run "+
					"deleted (%v)", name, deletedPods)
			}
		}
		for _, p := range livePodNames {
			if _, ok := pending[p]; !ok {
				problem = fmt.Sprintf("live pod %s has no consumer in group %s", p, consumerGroup)
			}
		}
		if problem == "" {
			sort.Strings(stale)
			return stale
		}
		if time.Now().After(deadline) {
			t.Fatalf("the %s consumer group never converged within %s: %s\nfull group: %v",
				consumerGroup, timeout, problem, pending)
		}
		time.Sleep(2 * time.Second)
	}
}

// --- results file -----------------------------------------------------------

const (
	resultsBegin = "<!-- BEGIN K6 MEASUREMENTS -->"
	resultsEnd   = "<!-- END K6 MEASUREMENTS -->"
)

// writeResults splices the measured lines into infra/k8s/RESULTS.md between its
// markers. The prose around them is hand-written and reviewed; only the numbers
// are generated, so the file can be read as a claim and audited as a record.
func writeResults(t *testing.T) {
	path := os.Getenv("K8S_RESULTS_FILE")
	if path == "" {
		t.Log("K8S_RESULTS_FILE unset; not recording measurements")
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("read results file: %v", err)
		return
	}
	body := string(raw)
	i, j := strings.Index(body, resultsBegin), strings.Index(body, resultsEnd)
	if i < 0 || j < 0 || j < i {
		t.Errorf("%s has no %s / %s marker pair", path, resultsBegin, resultsEnd)
		return
	}

	resultsMu.Lock()
	lines := append([]string(nil), results...)
	resultsMu.Unlock()
	verdict := "all four scenarios passed"
	if t.Failed() {
		verdict = "**INCOMPLETE — the run failed; the lines below cover only what was measured before the failure**"
	}

	var b strings.Builder
	b.WriteString(body[:i+len(resultsBegin)])
	b.WriteString("\n\nMeasured by `make k8s-failure` (`scripts/k8s/failure-check.sh` →\n")
	b.WriteString("`tests/system/k8s_failure_test.go`), " + verdict + ".\n\n")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	b.WriteString("\n")
	b.WriteString(body[j:])
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil { //nolint:gosec // tracked doc, not a secret
		t.Errorf("write results file: %v", err)
		return
	}
	t.Logf("recorded %d measurements into %s", len(lines), path)
}

// --- job preparation --------------------------------------------------------

// preparedJob is a submission with every slow step already done: the recipient
// resolved, the document encrypted, and the ciphertext PUT to object storage.
// All that is left is POST /jobs, which is what gets fired inside an outage
// window a second wide.
type preparedJob struct {
	req createJobRequest
}

// recipientOnce caches the recipient lookup and its public key across the whole
// run. Not an optimisation: /recipients and /jobs are behind the guest rate
// limit (20/min, burst 20), and re-resolving per job would spend the budget on
// nothing and turn a later submission into a 429 that looks like a failure.
var (
	recipientOnce sync.Once
	recipientID   string
	recipientPEM  string
)

func prepareJob(t *testing.T, baseURL string) preparedJob {
	t.Helper()
	recipientOnce.Do(func() {
		var recips []recipient
		getJSON(t, baseURL+"/recipients?q=Testmann", &recips)
		if len(recips) == 0 {
			t.Fatalf("no seeded recipient found (did scripts/e2e/seed.sh run?)")
		}
		recipientID = recips[0].RecipientID
		var pk pubKeyResp
		getJSON(t, baseURL+"/recipients/"+recipientID+"/public-key", &pk)
		recipientPEM = pk.PublicKeyPem
	})

	encKeyB64, ciphertext := encryptForPrinter(t, recipientPEM, makePDF())
	var up uploadURLResp
	if code := postJSON(t, baseURL+"/jobs/upload-url",
		map[string]string{"recipient_id": recipientID, "filename": "letter.pdf"}, &up); code != 200 {
		t.Fatalf("POST /jobs/upload-url: status %d", code)
	}
	putCiphertext(t, up.UploadURL, ciphertext)
	return preparedJob{req: createJobRequest{
		EncryptedKey: encKeyB64,
		BlobRef:      up.BlobRef,
		RecipientID:  recipientID,
		PageCount:    1,
	}}
}

func postPreparedJob(t *testing.T, baseURL string, p preparedJob) createJobResp {
	t.Helper()
	var job createJobResp
	code := postJSON(t, baseURL+"/jobs", p.req, &job)
	if code != 202 {
		t.Fatalf("POST /jobs: status %d, body %+v", code, job)
	}
	if job.JobID == "" || job.GuestToken == "" {
		t.Fatalf("POST /jobs returned no job id / guest token: %+v", job)
	}
	return job
}
