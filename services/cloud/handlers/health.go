package handlers

import "net/http"

// Healthz reports 503 if either backing store is unreachable, per
// plans/09-api-contracts.md. The earlier Phase 0 stub always returned 200;
// this replaces it now that the server actually has a DB and Redis to check.
func (s *Server) Healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.SQLDB.PingContext(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "postgres unreachable", "UNAVAILABLE")
		return
	}
	if err := s.Redis.Ping(r.Context()).Err(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "redis unreachable", "UNAVAILABLE")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
