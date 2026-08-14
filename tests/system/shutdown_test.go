//go:build shutdown

// Graceful-shutdown acceptance against the real stack (Goal K0,
// plans/16-kubernetes.md §4.1). scripts/shutdown/check.sh brings up the
// two-node stack and runs this; the consumer-group half of the acceptance
// (the `--scale 3` up/down/up cycle) lives in that script, where scaling is
// natural and Redis is one `docker compose exec` away.
//
// What this file proves is the half that needs a real client on a real socket:
// an open SSE stream is ended *deliberately* by the draining server, rather
// than severed when the process dies. The distinction is invisible to timing
// alone -- an unhandled SIGTERM also closes the connection instantly -- so the
// assertion is on the wire content: a `: draining` comment written by the
// handler, then a clean EOF. A killed process cannot produce that.
package system

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestShutdown_OpenSSEStreamIsDrainedNotSevered submits a job that will sit in
// a non-terminal state (the printer is stopped first, so nothing delivers it),
// holds its status stream open, then SIGTERMs the node serving that stream.
func TestShutdown_OpenSSEStreamIsDrainedNotSevered(t *testing.T) {
	ownerURL := env(t, "E2E_OWNER_URL")

	// Stop the printer so the job cannot reach a terminal status and close the
	// stream on us before the signal lands. This is the long-lived in-flight
	// request Shutdown alone cannot release.
	dockerCompose(t, "stop", "printer")
	t.Cleanup(func() { dockerCompose(t, "start", "printer") })

	job := submitEncryptedJob(t, ownerURL)
	t.Logf("submitted job %s (status %q) with the printer stopped", job.JobID, job.Status)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	url := fmt.Sprintf("%s/jobs/%s/stream?token=%s", ownerURL, job.JobID, job.GuestToken)
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
		t.Fatalf("SSE stream: status %d", resp.StatusCode)
	}

	br := bufio.NewReader(resp.Body)
	// The snapshot event proves the handler is parked in its select loop.
	snapshot, err := readUntilData(br)
	if err != nil {
		t.Fatalf("reading the snapshot event: %v", err)
	}
	t.Logf("stream open, snapshot: %s", snapshot)

	// SIGTERM the node serving this stream. `docker compose stop` sends
	// SIGTERM and waits stop_grace_period before SIGKILL -- never `kill -9`,
	// which would prove nothing about the drain path.
	stopped := make(chan error, 1)
	go func() {
		out, err := exec.Command("docker", composeArgs(t, "stop", "cloud-server")...).CombinedOutput()
		if err != nil {
			err = fmt.Errorf("%w: %s", err, out)
		}
		stopped <- err
	}()
	t.Cleanup(func() { dockerCompose(t, "start", "cloud-server") })

	start := time.Now()
	lines, err := readRemaining(br, 60*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("reading the stream tail: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("docker compose stop cloud-server: %v", err)
	}

	tail := strings.Join(lines, "\n")
	t.Logf("stream tail after SIGTERM (%s):\n%s", elapsed.Round(time.Millisecond), tail)
	if !strings.Contains(tail, ": draining") {
		t.Fatalf("stream ended without the `: draining` notice — it was severed, not drained. Tail: %q", tail)
	}
	// The point of the drain signal is that the handler returns at once
	// instead of pinning Shutdown for the whole grace period.
	if elapsed > 15*time.Second {
		t.Fatalf("stream took %s to end after SIGTERM; the drain signal should release it immediately", elapsed)
	}
}

// readUntilData reads lines until an SSE data line, returning its payload.
func readUntilData(br *bufio.Reader) (string, error) {
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return "", err
		}
		if after, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "data: "); ok {
			return after, nil
		}
	}
}

// readRemaining reads the rest of the response body until EOF (the server
// closing the stream) or the timeout, returning the lines seen.
func readRemaining(br *bufio.Reader, timeout time.Duration) ([]string, error) {
	type result struct {
		lines []string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		var lines []string
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				lines = append(lines, strings.TrimSuffix(line, "\n"))
			}
			if err != nil {
				if err == io.EOF {
					err = nil
				}
				done <- result{lines, err}
				return
			}
		}
	}()
	select {
	case r := <-done:
		return r.lines, r.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("stream did not end within %s", timeout)
	}
}
