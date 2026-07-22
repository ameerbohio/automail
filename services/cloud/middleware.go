package main

import (
	"crypto/rsa"
	"net/http"
	"strings"

	"automail/cloud/authctx"
	"automail/cloud/handlers"
	"automail/cloud/jwtutil"
)

// NodeHeader names the cloud node that served a response. plans/03-scaling.md
// runs N stateless cloud nodes behind Traefik, and NODE_ID already identifies
// each one as its Redis Streams consumer -- this just surfaces the same name
// over HTTP so the portal can show a sender which node took their submission.
//
// It is metadata only (a consumer name, never a secret) and it never carries
// anything about the document. It does disclose topology, though: see
// plans/13-v2-roadmap.md "Richer request-path observability" for why a
// non-demo deployment would gate this behind a flag or emit an opaque id.
const NodeHeader = "X-Automail-Node"

// nodeHeader stamps NodeHeader on every response. Wrapped around the whole
// mux, so it covers the SSE stream too -- the header is set before any
// handler writes, which is what Go requires for it to reach the wire.
func nodeHeader(nodeID string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(NodeHeader, nodeID)
			next.ServeHTTP(w, r)
		})
	}
}

// optionalAuth extracts sender identity from a Bearer token if present,
// without rejecting the request if it's absent or invalid -- POST /jobs
// and /jobs/upload-url are "auth optional" per plans/09-api-contracts.md:
// an authenticated sender gets sender_id set; everyone else is a guest.
func optionalAuth(pubKey *rsa.PublicKey) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authz := r.Header.Get("Authorization")
			if after, ok := strings.CutPrefix(authz, "Bearer "); ok {
				if claims, err := jwtutil.ParseAccessToken(pubKey, after); err == nil {
					r = r.WithContext(authctx.WithSender(r.Context(), claims.Subject, claims.Role))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireAuth rejects requests without a valid Bearer token. Wired to
// POST /auth/logout and the account history route; requireAdmin (below)
// layers a role check on top for the ops-dashboard endpoints.
func requireAuth(pubKey *rsa.PublicKey) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authz := r.Header.Get("Authorization")
			after, ok := strings.CutPrefix(authz, "Bearer ")
			if !ok {
				handlers.WriteError(w, http.StatusUnauthorized, "missing bearer token", "UNAUTHORIZED")
				return
			}
			claims, err := jwtutil.ParseAccessToken(pubKey, after)
			if err != nil {
				handlers.WriteError(w, http.StatusUnauthorized, "invalid or expired token", "UNAUTHORIZED")
				return
			}
			next.ServeHTTP(w, r.WithContext(authctx.WithSender(r.Context(), claims.Subject, claims.Role)))
		})
	}
}

// requireAdmin gates the Phase 9 ops-dashboard endpoints (GET /admin/*):
// a valid Bearer token AND an "admin" role claim (plans/07-ops-dashboard.md,
// plans/09-api-contracts.md). A missing/invalid token is 401 UNAUTHORIZED; a
// valid token for a non-admin (a regular sender) is 403 FORBIDDEN -- the
// request is authenticated, just not entitled. The admin role is never
// self-assignable: Register hard-codes role='sender', so an admin exists only
// if seeded directly in the database.
func requireAdmin(pubKey *rsa.PublicKey) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authz := r.Header.Get("Authorization")
			after, ok := strings.CutPrefix(authz, "Bearer ")
			if !ok {
				handlers.WriteError(w, http.StatusUnauthorized, "missing bearer token", "UNAUTHORIZED")
				return
			}
			claims, err := jwtutil.ParseAccessToken(pubKey, after)
			if err != nil {
				handlers.WriteError(w, http.StatusUnauthorized, "invalid or expired token", "UNAUTHORIZED")
				return
			}
			if claims.Role != "admin" {
				handlers.WriteError(w, http.StatusForbidden, "admin role required", "FORBIDDEN")
				return
			}
			next.ServeHTTP(w, r.WithContext(authctx.WithSender(r.Context(), claims.Subject, claims.Role)))
		})
	}
}
