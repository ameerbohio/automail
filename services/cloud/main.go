package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"log"
	"net/http"
	"os"
)

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func internalHealthzHandler(w http.ResponseWriter, r *http.Request) {
	// Reaching this handler at all proves the caller presented a client
	// certificate signed by the internal CA -- tls.RequireAndVerifyClientCert
	// below rejects the TLS handshake before any handler runs otherwise.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// mTLS listener: stands in for the Phase 3 printer-link (/internal/printer-link)
// so Phase 1's certs can be verified end-to-end now. Phase 3 reuses this same
// tls.Config when it upgrades /internal/printer-link to a WebSocket.
func startMTLSServer(addr string) error {
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

	server := &http.Server{
		Addr:      addr,
		Handler:   mux,
		TLSConfig: tlsConfig,
	}
	log.Printf("cloud-server internal mTLS listener on %s", addr)
	return server.ListenAndServeTLS(os.Getenv("MTLS_CLOUD_CERT_PATH"), os.Getenv("MTLS_CLOUD_KEY_PATH"))
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler)

	if mtlsPort := os.Getenv("MTLS_PORT"); mtlsPort != "" {
		go func() {
			if err := startMTLSServer(":" + mtlsPort); err != nil {
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
