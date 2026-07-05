package main

import (
	"crypto/rsa"
	"net/http"
	"strings"

	"automail/cloud/authctx"
	"automail/cloud/handlers"
	"automail/cloud/jwtutil"
)

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
// POST /auth/logout today; the account pages (Phase 8) and admin
// endpoints (Phase 9) attach it to their routes as they land.
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
