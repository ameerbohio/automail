//go:build integration

// Package main integration tests (build tag `integration`, run via
// `make test-integration` when a Docker daemon is present). Spec:
// docs/testing-plan.md Part 2 -- replace the fakes that hide risk with the
// real thing where the fake can't prove the property that matters:
//   - real Postgres: the pgcrypto extension loads, the audit-immutability
//     trigger actually rejects DELETE/UPDATE, and SELECT FOR UPDATE NOWAIT
//     returns the 55P03 lock error under contention instead of hanging.
//   - real Redis: Streams consumer-group XADD/XREADGROUP/XACK round-trips,
//     an un-ACKed entry is reclaimed by XAUTOCLAIM (the crash-recovery
//     path), and pub/sub crosses connections (the cross-node fan-out the
//     dispatch design depends on).
//   - real MinIO: a pre-signed PUT then GET round-trips ciphertext, and the
//     cloud code path never reads blob bytes.
//
// Each suite spins its own ephemeral container via testcontainers-go and
// tears it down through t.Cleanup, so nothing leaks between runs and the
// tests are hermetic. A fake proves the code calls the right method; only
// the real dependency proves Postgres honors NOWAIT or that a consumer
// group survives a crash -- that promotion from fake -> real is the whole
// point of this Part.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"automail/cloud/db"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Pinned images. Postgres/Redis match docker-compose.yml; MinIO uses the
// module's tested release tag (compose uses :latest, which Goal T12 pins
// for deploy parity -- a fixed tag here keeps the tests reproducible).
const (
	postgresImage = "postgres:16-alpine"
	redisImage    = "redis:7-alpine"
	minioImage    = "minio/minio:RELEASE.2024-01-16T16-07-38Z"

	containerStartTimeout = 120 * time.Second
)

// startPostgres brings up a Postgres container with schema.sql applied on
// init (the same /docker-entrypoint-initdb.d path the dev compose uses),
// and returns a ready *sql.DB plus the sqlc query layer bound to it.
func startPostgres(t *testing.T) (*sql.DB, *db.Queries) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), containerStartTimeout)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithInitScripts("db/schema.sql"),
		tcpostgres.WithDatabase("automail"),
		tcpostgres.WithUsername("automail"),
		tcpostgres.WithPassword("automail"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(containerStartTimeout),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres connection string: %v", err)
	}
	sqlDB, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open postgres: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	pingUntilReady(t, sqlDB)
	return sqlDB, db.New(sqlDB)
}

// pingUntilReady guards against the brief window where the container logs
// "ready" but the socket isn't accepting the app's connections yet.
func pingUntilReady(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := sqlDB.PingContext(ctx)
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("postgres never became ready: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// startRedis brings up a Redis container and returns a connected client.
func startRedis(t *testing.T) *redis.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), containerStartTimeout)
	defer cancel()

	ctr, err := tcredis.Run(ctx, redisImage)
	if err != nil {
		t.Fatalf("start redis container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	url, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis connection string: %v", err)
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	return rdb
}

// startMinio brings up a MinIO container and returns a connected client
// plus a teardown handle (so the torn-down-container test can kill it
// mid-suite). The default EnsureBucket is NOT called here -- suites that
// need the bucket create it, mirroring startup.
func startMinio(t *testing.T) (*minio.Client, *tcminio.MinioContainer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), containerStartTimeout)
	defer cancel()

	ctr, err := tcminio.Run(ctx, minioImage)
	if err != nil {
		t.Fatalf("start minio container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	endpoint, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("minio connection string: %v", err)
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(ctr.Username, ctr.Password, ""),
		Secure: false,
	})
	if err != nil {
		t.Fatalf("minio.New: %v", err)
	}
	return client, ctr
}

// seedJob inserts the minimal FK chain a jobs row needs (building ->
// mailbox -> slot) and one 'submitted' job, returning the job id. Used by
// the NOWAIT contention test. encrypted_key is arbitrary bytes -- these
// tests never decrypt it; they only exercise row locking.
func seedJob(t *testing.T, sqlDB *sql.DB) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	var buildingID, mailboxID, slotID, jobID uuid.UUID
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO buildings (address) VALUES ('123 Test St') RETURNING id`,
	).Scan(&buildingID); err != nil {
		t.Fatalf("seed building: %v", err)
	}
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO mailboxes (building_id, public_key_pem) VALUES ($1, 'PEM') RETURNING id`,
		buildingID,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("seed mailbox: %v", err)
	}
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO mailbox_slots (mailbox_id, slot_number) VALUES ($1, 1) RETURNING id`,
		mailboxID,
	).Scan(&slotID); err != nil {
		t.Fatalf("seed slot: %v", err)
	}
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO jobs (guest_token_hash, mailbox_id, slot_id, encrypted_key, blob_ref, page_count)
		 VALUES ('guesthash', $1, $2, $3, 'blobs/seed', 1) RETURNING id`,
		mailboxID, slotID, []byte("ciphertext-key-bytes"),
	).Scan(&jobID); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	return jobID
}

// seedAuditEvent inserts a job + one audit_events row and returns the audit
// row id, for the immutability-trigger test.
func seedAuditEvent(t *testing.T, sqlDB *sql.DB) uuid.UUID {
	t.Helper()
	jobID := seedJob(t, sqlDB)
	var auditID uuid.UUID
	if err := sqlDB.QueryRowContext(context.Background(),
		`INSERT INTO audit_events (job_id, action) VALUES ($1, 'job_submitted') RETURNING id`,
		jobID,
	).Scan(&auditID); err != nil {
		t.Fatalf("seed audit event: %v", err)
	}
	return auditID
}

// uniqueName returns a per-test-unique string for stream/channel names so
// suites sharing a container never collide.
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s:%s", prefix, uuid.NewString())
}
