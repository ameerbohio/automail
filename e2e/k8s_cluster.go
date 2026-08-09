//go:build k8sfail

// Cluster-manipulation and cluster-observation primitives for the Goal K6
// failure/rollout suite: kubectl, redis-cli, psql and docker, plus the small
// HTTP helpers the traffic generator needs.
//
// These live apart from harness.go because harness.go is deliberately
// deployment-agnostic -- it speaks the product's HTTP contract and knows only
// about docker-compose. Everything here knows it is talking to Kubernetes, and
// only the K6 suite does.
//
// Two observations are made with EVAL rather than a bare redis-cli command, and
// it is worth knowing why: redis-cli renders nested reply arrays (XPENDING's
// per-entry rows, XINFO CONSUMERS' field maps) as indented, numbered trees that
// are miserable to parse and change shape between versions. A three-line Lua
// script flattens the same reply into `field field field` lines at the source,
// so the parsing here cannot drift from the server's formatting.
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- kubectl ---------------------------------------------------------------

func namespace() string {
	if ns := os.Getenv("K8S_NAMESPACE"); ns != "" {
		return ns
	}
	return "automail"
}

// kubectlTry runs a cluster-scoped kubectl command and returns its combined
// output plus the error, for the callers that expect a failure (evictions the
// PDB must refuse) or that must not abort a cleanup path.
func kubectlTry(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("kubectl", args...).CombinedOutput()
	return string(out), err
}

// kubectlOut runs a cluster-scoped kubectl command and fails the test on error.
func kubectlOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := kubectlTry(t, args...)
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

// kubectlNS is kubectlOut with the suite's namespace prepended.
func kubectlNS(t *testing.T, args ...string) string {
	t.Helper()
	return kubectlOut(t, append([]string{"-n", namespace()}, args...)...)
}

func dockerCmd(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func nodeKubeletVersion(t *testing.T) string {
	t.Helper()
	return kubectlOut(t, "get", "nodes", "-o", "jsonpath={.items[0].status.nodeInfo.kubeletVersion}")
}

// --- pods, nodes, deployment ------------------------------------------------

// livePod is a cloud-server pod that is actually serving: Running and NOT
// terminating.
//
// THE TERMINATING POD IS THE TRAP. A pod being deleted keeps `status.phase:
// Running` for its whole 35s grace period, so a `--field-selector=
// status.phase=Running` query keeps returning the pod that a rollout has
// already replaced -- and "did the rollout replace every pod?" then reads as
// "no" for up to half a minute after it plainly did. The only reliable signal
// is `metadata.deletionTimestamp`, which no field selector exposes, so the pod
// list is fetched as JSON and filtered here.
type livePod struct {
	name string
	node string
}

func livePods(t *testing.T) []livePod {
	t.Helper()
	out := kubectlNS(t, "get", "pods", "-l", "app.kubernetes.io/name=cloud-server", "-o", "json")
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string `json:"name"`
				DeletionTimestamp string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("decode pod list: %v", err)
	}
	var pods []livePod
	for _, it := range list.Items {
		if it.Status.Phase != "Running" || it.Metadata.DeletionTimestamp != "" {
			continue
		}
		pods = append(pods, livePod{name: it.Metadata.Name, node: it.Spec.NodeName})
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].name < pods[j].name })
	return pods
}

func cloudPods(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, p := range livePods(t) {
		names = append(names, p.name)
	}
	return names
}

// podsOnNode lists the cloud-server pods currently scheduled on a node --
// after a drain this must be empty for that node.
func podsOnNode(t *testing.T, node string) []string {
	t.Helper()
	var names []string
	for _, p := range livePods(t) {
		if p.node == node {
			names = append(names, p.name)
		}
	}
	return names
}

// placement renders where the cloud-server replicas sit, as a stable string
// (`node=count`, sorted) so a before/after pair in the results file is
// diffable instead of being map-iteration noise.
func placement(t *testing.T) string {
	t.Helper()
	counts := map[string]int{}
	for _, p := range livePods(t) {
		counts[p.node]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	return strings.Join(parts, " ")
}

// drainableNode picks a node the drain scenario may safely evict everything
// from. Three constraints, every one of them learned rather than guessed:
//
//   - NEVER the control-plane/server node. The k3d-local overlay pins the data
//     tier there because `local-path` PVs carry a nodeAffinity to the node they
//     bound on; draining it strands Postgres in `Pending` forever and takes the
//     stack with it.
//   - NEVER the node running Traefik. It is a Deployment, so a drain evicts and
//     reschedules it perfectly well -- but that moves the INGRESS, and every
//     suite here reaches the cluster through it. The first K6 run drained the
//     Traefik node and the next scenario's traffic opened with a burst of EOFs
//     on connections to an ingress that no longer existed; measured as a
//     rolling-update failure, it was nothing of the kind. Moving the edge is a
//     real disruption, but it is a different one from "can the application tier
//     be replaced under load", and mixing them measures neither.
//   - Prefer an agent whose only kube-system pods are DaemonSet-owned (svclb).
//     Evicting CoreDNS or metrics-server is legal and they reschedule, but it
//     adds unrelated churn to a measurement about the application tier.
func drainableNode(t *testing.T) string {
	t.Helper()
	agents := nonEmptyLines(kubectlOut(t, "get", "nodes",
		"-l", "!node-role.kubernetes.io/control-plane",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}"))
	if len(agents) == 0 {
		t.Fatal("no agent nodes: refusing to drain the server node, which carries the data tier's local-path PVs")
	}

	ingress := map[string]bool{}
	for _, n := range nonEmptyLines(kubectlOut(t, "get", "pods", "-n", "kube-system",
		"-l", "app.kubernetes.io/name=traefik", "--field-selector=status.phase=Running",
		"-o", "jsonpath={range .items[*]}{.spec.nodeName}{\"\\n\"}{end}")) {
		ingress[n] = true
	}

	// node -> true when it hosts a kube-system pod that is not DaemonSet-owned.
	messy := map[string]bool{}
	sys := kubectlOut(t, "get", "pods", "-n", "kube-system",
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={range .items[*]}{.spec.nodeName}{\" \"}{.metadata.ownerReferences[0].kind}{\"\\n\"}{end}")
	for _, line := range nonEmptyLines(sys) {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] != "DaemonSet" {
			messy[f[0]] = true
		}
	}

	var fallback string
	for _, n := range agents {
		if len(podsOnNode(t, n)) == 0 || ingress[n] {
			continue // no cloud-server pod proves nothing; the ingress node is off limits
		}
		if !messy[n] {
			return n
		}
		fallback = n
	}
	if fallback == "" {
		t.Fatalf("no drainable agent node: every agent either runs no cloud-server pod or runs the "+
			"ingress (agents %v, ingress on %v)", agents, ingress)
	}
	t.Logf("every non-ingress agent hosting a cloud-server pod also hosts kube-system workloads; draining %s", fallback)
	return fallback
}

// waitEdgeStable requires a run of consecutive 200s through the ingress before
// a measurement starts. Scenarios run in sequence and the earlier ones move
// pods around; without this gate, churn one scenario caused is charged to the
// next one's numbers.
func waitEdgeStable(t *testing.T, url string, consecutive int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	streak := 0
	var lastErr string
	for time.Now().Before(deadline) {
		res, err := httpGetNoBody(url)
		switch {
		case err != nil:
			streak, lastErr = 0, err.Error()
		case res.status/100 != 2:
			streak, lastErr = 0, fmt.Sprintf("HTTP %d", res.status)
		default:
			streak++
			if streak >= consecutive {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the ingress never returned %d consecutive 2xx within %s (last problem: %s) -- "+
		"the measurement below would be charging earlier churn to this scenario", consecutive, timeout, lastErr)
}

func waitDeploymentReady(t *testing.T, timeout time.Duration) {
	t.Helper()
	out, err := kubectlTry(t, "-n", namespace(), "rollout", "status", "deploy/cloud-server",
		fmt.Sprintf("--timeout=%ds", int(timeout.Seconds())))
	if err != nil {
		t.Fatalf("cloud-server never became fully Ready within %s: %v\n%s", timeout, err, out)
	}
}

// --- PodDisruptionBudget ----------------------------------------------------

func pdbStatus(t *testing.T) (currentHealthy, desiredHealthy, disruptionsAllowed int) {
	t.Helper()
	out := kubectlNS(t, "get", "pdb", "cloud-server", "-o",
		"jsonpath={.status.currentHealthy} {.status.desiredHealthy} {.status.disruptionsAllowed}")
	f := strings.Fields(out)
	if len(f) != 3 {
		t.Fatalf("unexpected PDB status %q", out)
	}
	return atoi(t, f[0]), atoi(t, f[1]), atoi(t, f[2])
}

func waitPDBAllowed(t *testing.T, want int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, _, allowed := pdbStatus(t); allowed == want {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// evictPod asks the API server to evict a pod through the eviction subresource
// -- the only call a PodDisruptionBudget actually mediates. `kubectl delete
// pod` bypasses the budget entirely, which is why the refusal below has to be
// provoked this way and not with a delete.
func evictPod(t *testing.T, pod string) (string, error) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"apiVersion": "policy/v1",
		"kind":       "Eviction",
		"metadata":   map[string]string{"name": pod, "namespace": namespace()},
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp("", "eviction-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(body); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return kubectlTry(t, "create", "--raw",
		fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/eviction", namespace(), pod), "-f", f.Name())
}

// --- Redis ------------------------------------------------------------------

func chanDispatch() string { return "mailbox:" + mailboxIDVar + ":dispatch" }

func redisCLI(t *testing.T, args ...string) string {
	t.Helper()
	return kubectlNS(t, append([]string{"exec", "redis-0", "--", "redis-cli"}, args...)...)
}

// dispatchSubscribers counts the cloud pods currently subscribed to this
// mailbox's dispatch channel -- i.e. how many hold a live printer socket. This
// is the authoritative liveness signal, not `mailbox:<id>:state`: that key has
// a 90s TTL and outlives a dropped socket, whereas a subscriber count is the
// same thing `attemptDispatch` reacts to when Publish reports 0 receivers.
func dispatchSubscribers(t *testing.T) int {
	t.Helper()
	out := redisCLI(t, "PUBSUB", "NUMSUB", chanDispatch())
	f := strings.Fields(out)
	if len(f) == 0 {
		t.Fatalf("PUBSUB NUMSUB returned %q", out)
	}
	return atoi(t, f[len(f)-1])
}

func waitDispatchSubscribers(t *testing.T, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last int
	for time.Now().Before(deadline) {
		last = dispatchSubscribers(t)
		if last == want {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("subscribers on %s stayed at %d, want %d, after %s", chanDispatch(), last, want, timeout)
}

func streamLen(t *testing.T) int {
	t.Helper()
	return atoi(t, redisCLI(t, "XLEN", pendingStream))
}

// luaPendingEntries flattens XPENDING's per-entry rows into
// `<entry-id> <consumer> <job-id>` lines.
//
// The job id has to come along: `jobs:pending` accumulates every entry ever
// XADDed (ACK'd entries are not removed from a Stream), and previous runs leave
// their own entries behind, so "some consumer holds a pending entry" is not the
// same question as "the consumer holding MY job's entry". Each pending id is
// therefore looked up with XRANGE and its `job_id` field extracted.
//
// ONLY job_id LEAVES THIS SCRIPT. The stream entry also carries
// `encrypted_key`; it is never read, never returned and never logged, here or
// anywhere else in the test suite (CLAUDE.md, non-negotiable).
const luaPendingEntries = `
local ok, res = pcall(function()
  return redis.call('XPENDING', KEYS[1], ARGV[1], '-', '+', 50)
end)
if not ok then return '' end
local out = ''
for i = 1, #res do
  local id, consumer, job = res[i][1], res[i][2], ''
  local entry = redis.call('XRANGE', KEYS[1], id, id)
  if entry and entry[1] then
    local fields = entry[1][2]
    for j = 1, #fields, 2 do
      if fields[j] == 'job_id' then job = fields[j + 1] end
    end
  end
  out = out .. id .. ' ' .. consumer .. ' ' .. job .. '\n'
end
return out`

// luaConsumerPending flattens XINFO CONSUMERS into `<consumer> <pending>` lines.
const luaConsumerPending = `
local ok, res = pcall(function()
  return redis.call('XINFO', 'CONSUMERS', KEYS[1], ARGV[1])
end)
if not ok then return '' end
local out = ''
for i = 1, #res do
  local e, name, pending = res[i], '', '0'
  for j = 1, #e, 2 do
    if e[j] == 'name' then name = e[j + 1] end
    if e[j] == 'pending' then pending = tostring(e[j + 1]) end
  end
  out = out .. name .. ' ' .. pending .. '\n'
end
return out`

// consumerPending maps consumer name -> pending-entry count for every consumer
// still registered in the group. A consumer's name is the pod name (NODE_ID
// comes from the downward API), so this doubles as the leak check.
func consumerPending(t *testing.T) map[string]int {
	t.Helper()
	out := redisCLI(t, "EVAL", luaConsumerPending, "1", pendingStream, consumerGroup)
	m := map[string]int{}
	for _, line := range nonEmptyLines(out) {
		if f := strings.Fields(line); len(f) == 2 {
			m[f[0]] = atoi(t, f[1])
		}
	}
	return m
}

// waitForPELEntry blocks until some consumer holds THIS job's stream entry
// un-ACK'd, and returns that consumer, the entry id, and how long it took.
//
// The wait is real work, not slack: nothing publishes when a job is merely
// XADDed, so the entry sits untouched until a dispatcher's sweep ticker fires
// (dispatch.claimMinIdle, 60s) and reads it with XREADGROUP ">". Dispatch is
// still blocked at that point, so handle() leaves it un-ACK'd -- which is the
// state the reclaim scenario needs and the behaviour being asserted.
func waitForPELEntry(t *testing.T, jobID string, timeout time.Duration) (consumer, entryID string, waited time.Duration) {
	t.Helper()
	start := time.Now()
	deadline := start.Add(timeout)
	for time.Now().Before(deadline) {
		out := redisCLI(t, "EVAL", luaPendingEntries, "1", pendingStream, consumerGroup)
		for _, line := range nonEmptyLines(out) {
			if f := strings.Fields(line); len(f) == 3 && f[2] == jobID {
				return f[1], f[0], time.Since(start)
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("no consumer read job %s's entry into its PEL within %s -- with the sweep ticker at 60s "+
		"this should have happened well inside the window", jobID, timeout)
	return "", "", 0
}

// --- pod logs ---------------------------------------------------------------

// socketOwner reports which cloud-server pod logged the printer's registration
// most recently. kube-proxy chose it, so it must be discovered rather than
// assumed (plans/16-kubernetes.md §6.2); `since` bounds the scan so an older
// run's line can never win.
func socketOwner(t *testing.T, since string) string {
	t.Helper()
	marker := "printer-link: mailbox " + mailboxIDVar + " registered"
	var owner, ownerTS string
	for _, pod := range cloudPods(t) {
		out, err := kubectlTry(t, "-n", namespace(), "logs", pod, "--timestamps", "--since-time="+since)
		if err != nil {
			continue // pod may have gone away between listing and reading
		}
		for _, line := range nonEmptyLines(out) {
			if !strings.Contains(line, marker) {
				continue
			}
			ts := strings.Fields(line)[0]
			if ts > ownerTS {
				owner, ownerTS = pod, ts
			}
		}
	}
	if owner == "" {
		t.Fatalf("no cloud-server pod logged %q since %s", marker, since)
	}
	return owner
}

// reclaimEvidence looks for the XAUTOCLAIM log line naming this exact stream
// entry. Matching on the entry id rather than on the phrase alone is what makes
// this evidence: it ties the recovery to the job under test instead of to any
// reclaim that happened to occur during the run.
func reclaimEvidence(t *testing.T, entryID string) (pod, line string) {
	t.Helper()
	const marker = "reclaimed job from crashed consumer"
	for _, p := range cloudPods(t) {
		out, err := kubectlTry(t, "-n", namespace(), "logs", p)
		if err != nil {
			continue
		}
		for _, l := range nonEmptyLines(out) {
			if strings.Contains(l, marker) && strings.Contains(l, entryID) {
				return p, strings.TrimSpace(l)
			}
		}
	}
	return "", ""
}

// --- Postgres ---------------------------------------------------------------

// psqlCluster runs one query in the cluster's Postgres pod and returns the
// trimmed scalar result. kubectl exec has no -e flag, so PGPASSWORD is set by
// the command itself inside the pod -- the same shape scripts/e2e/seed.sh uses.
func psqlCluster(t *testing.T, query string) string {
	t.Helper()
	return kubectlNS(t, "exec", "postgres-0", "--",
		"env", "PGPASSWORD="+env(t, "E2E_PG_PASSWORD"),
		"psql", "-U", env(t, "E2E_PG_USER"), "-d", env(t, "E2E_PG_DB"), "-tAc", query)
}

// auditCount reads the append-only ledger. Exactly-once is asserted here rather
// than from a status field because the ledger cannot be UPDATE'd or DELETE'd
// (the trigger the T5 integration suite proved): two rows means the letter was
// printed twice, zero means the job evaporated.
func auditCount(t *testing.T, jobID, action string) int {
	t.Helper()
	// jobID is a server-assigned UUID and action a fixed constant, not user input.
	return atoi(t, psqlCluster(t,
		"SELECT count(*) FROM audit_events WHERE job_id='"+jobID+"' AND action='"+action+"'"))
}

func jobRowStatus(t *testing.T, jobID string) string {
	t.Helper()
	return psqlCluster(t, "SELECT status FROM jobs WHERE id='"+jobID+"'")
}

// waitJobStatus polls the job row until it reaches want. Used instead of the
// SSE stream where the wait is minutes long (the XAUTOCLAIM window): a
// multi-minute idle SSE connection would be testing Traefik's idle timeout as
// much as the product.
func waitJobStatus(t *testing.T, jobID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = jobRowStatus(t, jobID)
		if last == want {
			return
		}
		if last == "failed" {
			t.Fatalf("job %s reached terminal status %q, want %q", jobID, last, want)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("job %s stuck in %q after %s, want %q", jobID, last, timeout, want)
}

func assertDeliveredExactlyOnce(t *testing.T, jobID string, trail []string) {
	t.Helper()
	if len(trail) == 0 || trail[len(trail)-1] != "delivered" {
		t.Fatalf("job %s did not reach delivered (trail %v)", jobID, trail)
	}
	if n := auditCount(t, jobID, "job_delivered"); n != 1 {
		t.Fatalf("job %s has %d job_delivered audit rows, want exactly 1 (0 = lost, >1 = double-printed)",
			jobID, n)
	}
	if st := jobRowStatus(t, jobID); st != "delivered" {
		t.Fatalf("job %s row status is %q, want \"delivered\"", jobID, st)
	}
}

// --- HTTP -------------------------------------------------------------------

type probeResult struct {
	status int
	node   string // X-Automail-Node: which pod answered
}

// httpGetNoBody is the traffic generator's request: it drains and closes the
// body so the keep-alive connection is reused, which is what makes a rolling
// update's endpoint changes visible instead of hidden behind a fresh dial per
// request.
func httpGetNoBody(url string) (probeResult, error) {
	resp, err := http.Get(url) //nolint:gosec // test-controlled edge URL
	if err != nil {
		return probeResult{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return probeResult{status: resp.StatusCode, node: resp.Header.Get("X-Automail-Node")}, nil
}

func putCiphertext(t *testing.T, url string, ciphertext []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(string(ciphertext)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT ciphertext to object storage: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload PUT: status %d: %s", resp.StatusCode, body)
	}
}

// --- small utilities --------------------------------------------------------

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimRight(l, "\r"); strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		t.Fatalf("expected a number, got %q: %v", s, err)
	}
	return n
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func intersect(a, b []string) []string {
	var out []string
	for _, x := range a {
		if containsStr(b, x) {
			out = append(out, x)
		}
	}
	return out
}
