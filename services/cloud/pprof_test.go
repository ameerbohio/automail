package main

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// freeAddr reserves a loopback port and releases it, so startPprof can bind it.
// Mildly racy in principle, harmless in practice for a local test.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return addr
}

// TestStartPprof_DisabledByDefault covers the production path: PPROF_ADDR is
// unset everywhere except the load profile, so startPprof must return without
// binding anything. The profiler dumps process memory, so "off unless explicitly
// asked for" is a security property, not just a config nicety.
func TestStartPprof_DisabledByDefault(t *testing.T) {
	addr := freeAddr(t)

	startPprof("") // must be a no-op

	// If it had bound anything it would not be this address, but prove the
	// no-op differently: the port stays free, so we can still bind it ourselves.
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("startPprof(\"\") appears to have started a listener: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestStartPprof_ServesOnlyProfilerEndpoints covers the enabled path and pins
// two contracts: the goroutine profile's first line is what scripts/load/run.sh
// parses to sample goroutine counts, and the private mux must expose *nothing*
// but the profiler (it is deliberately not the public mux and not
// DefaultServeMux).
func TestStartPprof_ServesOnlyProfilerEndpoints(t *testing.T) {
	addr := freeAddr(t)
	startPprof(addr)
	base := "http://" + addr

	client := &http.Client{Timeout: 2 * time.Second}

	var resp *http.Response
	var err error
	for i := 0; i < 100; i++ {
		resp, err = client.Get(base + "/debug/pprof/goroutine?debug=1")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("pprof listener never came up on %s: %v", addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("goroutine profile: status %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	// scripts/load/run.sh's pprof_goroutines() matches "total <N>" off this
	// output -- if the format ever changes, the load harness silently reads 0.
	if !strings.Contains(string(body), "goroutine profile: total") {
		t.Fatalf("unexpected goroutine profile body: %.120q", body)
	}

	// Anything outside /debug/pprof/ must 404: the explicit registration in
	// startPprof is what keeps the exposed surface to exactly the profiler.
	other, err := client.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer other.Body.Close()
	if other.StatusCode != http.StatusNotFound {
		t.Fatalf("profiler mux served %q with status %d; it must expose only /debug/pprof/*",
			"/", other.StatusCode)
	}
}
