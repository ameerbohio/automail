// Package handlers implements the cloud server's external HTTP endpoints.
// Every handler here is part of the zero-knowledge trust boundary: none of
// them ever decrypt, log, or forward encrypted_key anywhere but straight
// through to Postgres (plans/02-security.md "Zero-Knowledge Guarantee").
package handlers

import (
	"crypto/rsa"
	"database/sql"

	"automail/cloud/db"
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
	AppKey  string // pgcrypto symmetric key for PII columns

	JWTPriv *rsa.PrivateKey
	JWTPub  *rsa.PublicKey

	Hub *link.Hub // printer-link connection registry + dispatch routing (Phase 3)
}
