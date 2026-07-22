package main

import (
	"log"
	"net/http"
	"net/http/pprof" // handlers registered explicitly on a private mux below
	"time"
)

// startPprof serves Go's runtime profiler (heap, goroutine, allocs, CPU) on a
// dedicated listener, but only when addr is non-empty. It is enabled solely by
// docker-compose.load.yml for Goal T10 load runs (testing-plan Part 8) so a load
// script can snapshot goroutine/heap counts; the base compose and any deploy
// host leave PPROF_ADDR unset, so this returns immediately and the profiler is
// never exposed. Kept on its own listener (not the public mux) because pprof
// dumps process memory and must never be routable from the internet.
func startPprof(addr string) {
	if addr == "" {
		return
	}
	mux := http.NewServeMux()
	// Explicit registration (rather than serving DefaultServeMux) keeps the
	// exposed set to exactly the profiler endpoints.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	log.Printf("pprof profiler listening on %s (load-profile only)", addr)
	go func() {
		server := &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		// #nosec G114 -- profiler listener; ReadHeaderTimeout is set above.
		if err := server.ListenAndServe(); err != nil {
			log.Printf("pprof listener stopped: %v", err)
		}
	}()
}
