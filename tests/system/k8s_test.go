//go:build k8s

// Kubernetes cluster E2E (Goal K5, plans/16-kubernetes.md §6). One encrypted
// job driven from outside the cluster, through the Traefik ingress, to a
// printer that is NOT a workload -- and a second job that proves the Redis
// fan-in across pods.
//
// What is different here, and why it is worth a separate suite from T8/T12:
//
//   - THE PRINTER IS OUTSIDE. It is a plain container on the host in its own
//     Compose project (infra/compose/k8s-printer.yml), dialing *out* to the
//     mTLS NodePort on wss://localhost:9843. That is the real architecture --
//     software inside a physical mailbox unit behind NAT -- and it is why the
//     printer is not, and must never be, a Deployment: two replicas sharing a
//     MAILBOX_ID both receive `mailbox:<id>:dispatch` and print the same letter
//     twice (§6). "Not a workload" is a design claim, and this suite is what
//     makes it an exercised one.
//
//   - THE SOCKET OWNER IS ARBITRARY. In Compose the printer dials a fixed
//     service alias, so the owner is known by construction. Here kube-proxy
//     picks a pod, so scripts/k8s/e2e.sh has to *discover* the owner from the
//     pods' logs and port-forward to a different one -- the method
//     plans/16-kubernetes.md §6.2 requires the goal to state, because a Service
//     cannot be addressed per-pod.
//
// Run it with `make k8s-e2e`, never bare `go test`: the script seeds the
// cluster's Postgres, starts the printer, resolves the owner/non-owner pods and
// sets every env knob below.
package system

import (
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestMain points the shared edge transport at the k3d ingress. Same routed
// hostnames and the same self-signed edge certificate as the Compose deploy
// smoke -- only the published port differs (9443 here, because Windows holds
// 443 and the Compose stack owns 8443 on this host). That the identical suite
// plumbing works against both targets is itself the base+overlay payoff.
func TestMain(m *testing.M) {
	root := os.Getenv("E2E_REPO_ROOT")
	if root == "" {
		fmt.Fprintln(os.Stderr, "E2E_REPO_ROOT is unset -- run via `make k8s-e2e`, not bare `go test`")
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
	os.Exit(m.Run())
}

// TestK8sJobThroughIngressDelivers is the Goal K5 acceptance proper: a guest
// submission that enters the cluster the way a browser's would -- TLS to the
// Traefik ingress on api.automail.local, a pre-signed PUT of ciphertext to
// object storage, POST /jobs -- comes back out as "delivered" from a printer
// that lives outside the cluster entirely, and leaves nothing in /dev/shm.
//
// The status trail is read over SSE rather than polled, so the delivered event
// is the one the cloud published on `job:<id>:status` after the printer's frame
// arrived on its socket. Polling would pass even if the fan-out were broken.
func TestK8sJobThroughIngressDelivers(t *testing.T) {
	apiURL := "https://" + apiHost

	job := submitEncryptedJob(t, apiURL)
	t.Logf("submitted job %s through the ingress (status %q)", job.JobID, job.Status)

	statuses := streamToTerminal(t, apiURL, job.JobID, job.GuestToken, 120*time.Second)
	if len(statuses) == 0 {
		t.Fatal("SSE stream through the ingress produced no events")
	}
	if last := statuses[len(statuses)-1]; last != "delivered" {
		t.Fatalf("job ended in %q, want \"delivered\" (full trail: %v)\n"+
			"if it stalled in \"queued\", the printer's SLOT_ID does not match the seeded "+
			"mailbox_slots.id; if it stalled in \"dispatching\", the printer could not fetch "+
			"the blob -- check that `minio` resolves to the k3d node hostPort in the container",
			last, statuses)
	}
	t.Logf("status trail through the ingress: %v", statuses)

	// The printer is a container, not a pod, so this reaches it with `docker
	// exec` (E2E_PRINTER_CONTAINER) rather than kubectl -- which is precisely
	// the point of the goal.
	assertDevShmClean(t)
	t.Log("printer wipe OK: /dev/shm holds no automail job file after delivered")
}

// TestK8sFanInFromNonOwnerPod proves the distributed property the printer
// boundary exists to make interesting: a job submitted to a cloud-server pod
// that does NOT hold the printer's socket still reaches the printer.
//
// The proof is the status POST /jobs returns. TryDispatch publishes to
// `mailbox:<id>:dispatch` and counts subscribers: with none it gives up and
// leaves the job "queued" for the Streams retry path. So "dispatching" from a
// pod with no local socket can only mean the OWNER pod -- on another node --
// received that publish over Redis and relayed it down its own connection.
// One field, no timing assumptions.
//
// scripts/k8s/e2e.sh identifies the owner from the pods' logs and port-forwards
// to a different one, because a ClusterIP Service load-balances and cannot be
// addressed per-pod (§6.2). The URL below is that forward.
func TestK8sFanInFromNonOwnerPod(t *testing.T) {
	nonOwnerURL := env(t, "K8S_NONOWNER_URL")
	ownerPod := env(t, "K8S_OWNER_POD")
	nonOwnerPod := env(t, "K8S_NONOWNER_POD")

	// Guard the guard: if the forward were pointed at the owner after all, the
	// assertion below would pass for the wrong reason and prove nothing. The
	// pod answers with its own NODE_ID (downward API, K3), so ask it.
	resp, err := http.Get(nonOwnerURL + "/healthz")
	if err != nil {
		t.Fatalf("GET %s/healthz: %v (is the kubectl port-forward still up?)", nonOwnerURL, err)
	}
	resp.Body.Close()
	served := resp.Header.Get("X-Automail-Node")
	if served != nonOwnerPod {
		t.Fatalf("the port-forward at %s is served by pod %q, expected %q -- "+
			"the fan-in assertion would not be measuring a non-owner", nonOwnerURL, served, nonOwnerPod)
	}
	if served == ownerPod {
		t.Fatalf("pod %q both holds the printer socket and is the fan-in target", served)
	}
	t.Logf("socket owner: %s; submitting to non-owner: %s", ownerPod, served)

	job := submitEncryptedJob(t, nonOwnerURL)
	if job.Status != "dispatching" {
		t.Fatalf("FAN-IN: POST /jobs on non-owner pod %s returned status %q, want \"dispatching\"\n"+
			"\"queued\" means the publish found zero subscribers -- the owner pod never relayed it, "+
			"so cross-pod fan-in is broken (or the printer is not registered/idle)", served, job.Status)
	}
	t.Logf("fan-in OK: job %s submitted on %s dispatched via the socket on %s", job.JobID, served, ownerPod)

	// Fan-out, same crossing in reverse: the terminal status is published by
	// the owner pod and read here from the non-owner's SSE stream.
	statuses := streamToTerminal(t, nonOwnerURL, job.JobID, job.GuestToken, 120*time.Second)
	if len(statuses) == 0 {
		t.Fatal("SSE stream on the non-owner pod produced no events")
	}
	if last := statuses[len(statuses)-1]; last != "delivered" {
		t.Fatalf("job ended in %q, want \"delivered\" (full trail: %v)", last, statuses)
	}
	t.Logf("fan-out OK: non-owner pod streamed %v ending in delivered", statuses)

	assertDevShmClean(t)
}
