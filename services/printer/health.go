package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"
)

// connected is flipped by the WebSocket client as it dials/reconnects.
// Docker's healthcheck hits /healthz on LISTEN_ADDR; it carries no job
// traffic and needs no auth -- it only proves the process is alive and,
// best-effort, that the link to the cloud server is currently up
// (plans/04-printer-microservice.md "The only locally bound port").
var connected atomic.Bool

func setConnected(v bool) {
	connected.Store(v)
}

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":    "ok",
		"connected": connected.Load(),
	})
}

// startHealthServer runs the local, unauthenticated healthcheck listener.
// Always 200 on "status" -- "connected": false just reports the printer
// is mid-reconnect, it isn't itself a failure (Docker shouldn't restart a
// healthy process that's waiting out a backoff).
func startHealthServer(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthzHandler)
	log.Printf("printer: healthcheck listener on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("healthcheck listener failed: %v", err)
	}
}
