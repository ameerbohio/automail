// Package handlers implements the cloud server's external HTTP endpoints.
// Every handler here is part of the zero-knowledge trust boundary: none of
// them ever decrypt, log, or forward encrypted_key anywhere but straight
// through to Postgres (plans/02-security.md "Zero-Knowledge Guarantee").
package handlers

import (
	"crypto/rsa"
	"database/sql"

	"automail/cloud/db"
	"automail/cloud/dispatch"
	"automail/cloud/link"

	"github.com/minio/minio-go/v7"
	"github.com/redis/go-redis/v9"
)

// Server holds every dependency a handler might need. Constructed once in
// main.go and passed to route registration -- handlers are methods on it
// so they don't reach for package-level globals.
type Server struct {
	Queries *db.Queries
	SQLDB   *sql.DB
	Redis   *redis.Client
	Minio   *minio.Client
	// UploadPresigner signs the browser-facing PUT URL. It is usually the same
	// client as Minio, but in deployments where object storage has a separate
	// public endpoint (the browser cannot reach the internal `minio:9000`
	// service name, only a public host), this is configured against that public
	// endpoint so the SigV4 host in the signed URL matches what the browser
	// sends. Server-side blob ops (BlobExists, RemoveBlob, the dispatch read
	// URL the in-network printer fetches) always use the internal Minio client.
	// If nil, callers fall back to Minio.
	UploadPresigner *minio.Client
	AppKey          string // pgcrypto symmetric key for PII columns

	JWTPriv *rsa.PrivateKey
	JWTPub  *rsa.PublicKey

	Hub        *link.Hub     // printer-link connection registry + dispatch routing (Phase 3)
	Dispatcher dispatch.Deps // immediate-dispatch dependencies, used by CreateJob (Phase 4)

	// Drain is closed by main.go when the process receives SIGTERM. It is the
	// explicit shutdown signal http.Server.Shutdown does *not* give handlers:
	// Shutdown waits for in-flight requests but never cancels their contexts,
	// so a long-lived handler (StreamJob) would pin the drain for the whole
	// grace period and then be severed anyway (plans/16-kubernetes.md §4.1).
	// Handlers that block should select on this and return deliberately.
	//
	// A nil channel blocks forever, which is exactly the right default: tests
	// and any caller that never shuts down behave as they did before.
	Drain <-chan struct{}
}

// draining reports whether shutdown has begun. Used by the readiness probe so
// Kubernetes stops routing new traffic here while the drain runs.
func (s *Server) draining() bool {
	select {
	case <-s.Drain:
		return true
	default:
		return false
	}
}

// uploadPresigner returns the MinIO client used to sign the browser-facing
// upload PUT URL: the dedicated public-endpoint client when configured,
// otherwise the internal client. Server-side blob ops always use s.Minio.
func (s *Server) uploadPresigner() *minio.Client {
	if s.UploadPresigner != nil {
		return s.UploadPresigner
	}
	return s.Minio
}
