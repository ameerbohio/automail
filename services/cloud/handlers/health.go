package handlers

import "net/http"

// Healthz reports 503 if either backing store is unreachable, per
// plans/09-api-contracts.md. The earlier Phase 0 stub always returned 200;
// this replaces it now that the server actually has a DB and Redis to check.
//
// This is the *readiness* probe (plans/16-kubernetes.md §4.4): it answers
// "should traffic be routed here", which is why it checks dependencies and
// why it starts failing the moment shutdown begins -- an endpoints controller
// (or Traefik's health check) that keeps sending requests to a draining
// process is the thing that turns a rolling update into dropped requests.
// Liveness is a different question; see Livez.
func (s *Server) Healthz(w http.ResponseWriter, r *http.Request) {
	if s.draining() {
		WriteError(w, http.StatusServiceUnavailable, "shutting down", "DRAINING")
		return
	}
	if err := s.SQLDB.PingContext(r.Context()); err != nil {
		WriteError(w, http.StatusServiceUnavailable, "postgres unreachable", "UNAVAILABLE")
		return
	}
	if err := s.Redis.Ping(r.Context()).Err(); err != nil {
		WriteError(w, http.StatusServiceUnavailable, "redis unreachable", "UNAVAILABLE")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Livez is the liveness probe: it answers only "is this process still
// serving its mux", never "is the world healthy" (plans/16-kubernetes.md
// §4.4). It touches no dependency on purpose -- putting the dependency-
// checking Healthz on liveness would make a Redis blip restart every
// cloud-server pod at once, converting a dependency wobble into a total
// outage.
//
// It deliberately keeps returning 200 while draining: a process that is
// shutting down gracefully is alive and must be left alone to finish. Saying
// otherwise gets it SIGKILLed mid-drain.
func (s *Server) Livez(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
