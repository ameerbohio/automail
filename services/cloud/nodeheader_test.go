package main

// The node header is what lets the portal show a sender which of the N
// stateless cloud nodes took their submission (plans/03-scaling.md; the
// "Richer request-path observability" note in plans/13-v2-roadmap.md).
// It is wrapped around the whole mux in main.go, so these tests pin the two
// properties that matter: it is stamped on every response including errors and
// streams, and it carries the node's name and nothing else.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNodeHeader_StampedOnEveryResponse(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"success", http.StatusOK, `{"status":"ok"}`},
		{"client error", http.StatusBadRequest, `{"error":"bad"}`},
		{"server error", http.StatusInternalServerError, `{"error":"boom"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			rec := httptest.NewRecorder()
			nodeHeader("cloud-server-2")(inner).ServeHTTP(rec, httptest.NewRequest("GET", "/anything", nil))

			if got := rec.Header().Get(NodeHeader); got != "cloud-server-2" {
				t.Fatalf("%s = %q, want %q", NodeHeader, got, "cloud-server-2")
			}
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if rec.Body.String() != tc.body {
				t.Fatalf("body = %q, want %q", rec.Body.String(), tc.body)
			}
		})
	}
}

// The middleware must set the header BEFORE the wrapped handler writes -- a
// header set after the first write never reaches the wire, which would silently
// drop it on the SSE stream (the handler writes headers immediately, then
// flushes frames for the life of the connection).
func TestNodeHeader_SetBeforeHandlerWrites(t *testing.T) {
	var seen string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = w.Header().Get(NodeHeader) // what a streaming handler would observe
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {}\n\n"))
	})
	rec := httptest.NewRecorder()
	nodeHeader("node-A")(inner).ServeHTTP(rec, httptest.NewRequest("GET", "/jobs/x/stream", nil))

	if seen != "node-A" {
		t.Fatalf("header visible to handler = %q, want %q", seen, "node-A")
	}
	if got := rec.Header().Get(NodeHeader); got != "node-A" {
		t.Fatalf("header on response = %q, want %q", got, "node-A")
	}
}

// The header is metadata only: it must carry the node name verbatim and must
// not be used to smuggle anything else onto the response.
func TestNodeHeader_CarriesOnlyTheNodeName(t *testing.T) {
	rec := httptest.NewRecorder()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	nodeHeader("node-A")(inner).ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))

	if len(rec.Header().Values(NodeHeader)) != 1 {
		t.Fatalf("expected exactly one %s value, got %v", NodeHeader, rec.Header().Values(NodeHeader))
	}
}
