package main

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"os"

	"automail/cloud/db"
	"automail/cloud/dispatch"
	"automail/cloud/handlers"
	"automail/cloud/link"
	"automail/cloud/minioclient"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/redis/go-redis/v9"
)

func loadRSAPrivateKey(path string) (*rsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in %s", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s is not an RSA private key", path)
	}
	return rsaKey, nil
}

func loadRSAPublicKey(path string) (*rsa.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in %s", path)
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%s is not an RSA public key", path)
	}
	return rsaKey, nil
}

func internalHealthzHandler(w http.ResponseWriter, r *http.Request) {
	// Reaching this handler at all proves the caller presented a client
	// certificate signed by the internal CA -- tls.RequireAndVerifyClientCert
	// below rejects the TLS handshake before any handler runs otherwise.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// startMTLSServer is the internal listener every printer dials out to.
// Phase 1 only proved certs verify end-to-end against /internal/healthz;
// Phase 3 adds the real /internal/printer-link WebSocket upgrade onto the
// same tls.Config and mux.
func startMTLSServer(addr string, srv *handlers.Server) error {
	caCert, err := os.ReadFile(os.Getenv("MTLS_CA_CERT_PATH"))
	if err != nil {
		return err
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		log.Fatal("failed to parse internal CA cert")
	}

	tlsConfig := &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  caPool,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/internal/healthz", internalHealthzHandler)
	mux.HandleFunc("GET /internal/printer-link", srv.PrinterLink)

	server := &http.Server{
		Addr:      addr,
		Handler:   mux,
		TLSConfig: tlsConfig,
	}
	log.Printf("cloud-server internal mTLS listener on %s", addr)
	return server.ListenAndServeTLS(os.Getenv("MTLS_CLOUD_CERT_PATH"), os.Getenv("MTLS_CLOUD_KEY_PATH"))
}

func mustEnv(key string) string {
	val := os.Getenv(key)
	if val == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return val
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	sqlDB, err := sql.Open("postgres", mustEnv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("postgres open: %v", err)
	}
	defer sqlDB.Close()

	redisOpts, err := redis.ParseURL(mustEnv("REDIS_URL"))
	if err != nil {
		log.Fatalf("redis URL: %v", err)
	}
	rdb := redis.NewClient(redisOpts)
	defer rdb.Close()

	minioClient, err := minio.New(mustEnv("MINIO_ENDPOINT"), &minio.Options{
		Creds:  credentials.NewStaticV4(mustEnv("MINIO_ACCESS_KEY"), mustEnv("MINIO_SECRET_KEY"), ""),
		Secure: os.Getenv("MINIO_SECURE") == "true",
	})
	if err != nil {
		log.Fatalf("minio client: %v", err)
	}
	if err := minioclient.EnsureBucket(context.Background(), minioClient); err != nil {
		log.Fatalf("minio bucket: %v", err)
	}

	jwtPriv, err := loadRSAPrivateKey(mustEnv("JWT_PRIVATE_KEY_PATH"))
	if err != nil {
		log.Fatalf("JWT private key: %v", err)
	}
	jwtPub, err := loadRSAPublicKey(mustEnv("JWT_PUBLIC_KEY_PATH"))
	if err != nil {
		log.Fatalf("JWT public key: %v", err)
	}

	queries := db.New(sqlDB)
	dispatchDeps := dispatch.Deps{
		SQLDB:   sqlDB,
		Queries: queries,
		Redis:   rdb,
		Minio:   minioClient,
	}
	srv := &handlers.Server{
		Queries:    queries,
		SQLDB:      sqlDB,
		Redis:      rdb,
		Minio:      minioClient,
		AppKey:     mustEnv("APP_ENCRYPTION_KEY"),
		JWTPriv:    jwtPriv,
		JWTPub:     jwtPub,
		Hub:        link.NewHub(rdb, queries),
		Dispatcher: dispatchDeps,
	}

	// nodeID identifies this process as a Redis Streams consumer
	// (plans/05-cloud-server.md NODE_ID env var) -- defaults to the
	// container hostname, which Docker Compose already makes unique per
	// replica under --scale.
	nodeID := os.Getenv("NODE_ID")
	if nodeID == "" {
		if hostname, err := os.Hostname(); err == nil {
			nodeID = hostname
		} else {
			nodeID = uuid.New().String()
		}
	}
	dispatcher := &dispatch.Dispatcher{Deps: dispatchDeps, NodeID: nodeID}
	if err := dispatcher.EnsureGroup(context.Background()); err != nil {
		log.Fatalf("dispatch: create consumer group: %v", err)
	}
	go dispatcher.Run(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", srv.Healthz)
	mux.HandleFunc("GET /recipients", srv.SearchRecipients)
	mux.HandleFunc("GET /recipients/{id}/public-key", srv.RecipientPublicKey)

	mux.Handle("POST /jobs/upload-url", optionalAuth(jwtPub)(http.HandlerFunc(srv.UploadURL)))
	mux.Handle("POST /jobs", optionalAuth(jwtPub)(http.HandlerFunc(srv.CreateJob)))
	// optionalAuth, not requireAuth: a Bearer JWT is only one of the two
	// accepted credentials -- guests authenticate with ?token= instead,
	// and StreamJob itself enforces that at least one checks out
	// (plans/09-api-contracts.md "GET /jobs/:id/stream").
	mux.Handle("GET /jobs/{id}/stream", optionalAuth(jwtPub)(http.HandlerFunc(srv.StreamJob)))

	mux.HandleFunc("POST /auth/login", srv.Login)
	mux.HandleFunc("POST /auth/refresh", srv.Refresh)
	mux.Handle("POST /auth/logout", requireAuth(jwtPub)(http.HandlerFunc(srv.Logout)))

	if mtlsPort := os.Getenv("MTLS_PORT"); mtlsPort != "" {
		go func() {
			if err := startMTLSServer(":"+mtlsPort, srv); err != nil {
				log.Fatal(err)
			}
		}()
	}

	addr := ":" + port
	log.Printf("cloud-server listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
