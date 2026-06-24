package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// newMTLSClientConfig builds the tls.Config used to dial the cloud
// server's printer-link listener. It presents this service's client
// certificate (signed by the internal CA) and verifies the cloud
// server's certificate against the same CA -- mTLS is mutual: each side
// authenticates the other, not just the printer authenticating to the
// cloud (plans/02-security.md "mTLS on every internal hop").
func newMTLSClientConfig(cfg config) (*tls.Config, error) {
	caCert, err := os.ReadFile(cfg.MTLSCACertPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse internal CA cert at %s", cfg.MTLSCACertPath)
	}

	cert, err := tls.LoadX509KeyPair(cfg.MTLSCertPath, cfg.MTLSKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load client cert/key: %w", err)
	}

	return &tls.Config{
		RootCAs:      caPool,
		Certificates: []tls.Certificate{cert},
	}, nil
}
