//go:build smoke || k8s || k8sfail

// The HTTPS-edge plumbing shared by the suites that drive the product the
// way a browser does -- through Traefik, on the routed hostnames, over the
// self-signed edge certificate. Goal T12's deployment-parity smoke
// (`deploy_smoke_test.go`, Compose), Goal K5's cluster E2E (`k8s_test.go`) and
// Goal K6's failure/rollout suite (`k8s_failure_test.go`, both k3d ingress)
// differ in where the edge lives and on which port, and in nothing
// else -- so the transport that makes `https://api.automail.local/...` reach it
// belongs here rather than being written twice.
package system

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// The production hostnames Traefik routes. These are the names the browser uses
// and the names the edge certificate must carry SANs for; nothing here may fall
// back to localhost or a published port, or the suites stop testing the edge.
const (
	portalHost = "automail.local"
	apiHost    = "api.automail.local"
	blobHost   = "blob.automail.local"
)

// edgeConfig is resolved once per suite, in TestMain.
type edgeConfig struct {
	httpsPort string         // host port the edge's websecure entrypoint is published on
	edgePool  *x509.CertPool // trusts the self-signed edge cert (browsers get a warning, not a failure)
	dialAddr  string         // where the routed hostnames actually live
}

var edge edgeConfig

// installEdgeTransport wires the whole suite to the edge. It installs a
// DefaultTransport that (a) trusts the self-signed edge cert and (b) maps every
// routed hostname to the published port -- the programmatic equivalent of curl's
// `--resolve`, and of the hosts-file entry a real deploy needs
// (docs/deploy-checklist.md). Doing it at the transport layer means the shared
// harness helpers drive real `https://api.automail.local/...` URLs with no
// knowledge that an edge is involved, so what they exercise is what a browser
// would.
//
// InsecureSkipVerify is deliberately NOT used: the cert is self-signed, not
// invalid, and pinning it here is what makes "the edge serves the cert we
// generated, for the SNI we asked for" an assertion rather than an assumption.
// The same infra/traefik/edge-cert.pem backs both targets -- Compose mounts it,
// and scripts/k8s/secrets.sh loads it into the cluster's TLS Secret -- so one
// pool is correct for both.
func installEdgeTransport(repoRoot, host, port string) error {
	pem, err := os.ReadFile(filepath.Join(repoRoot, "infra", "traefik", "edge-cert.pem"))
	if err != nil {
		return fmt.Errorf("read edge cert: %w (run ./infra/certs/gen-edge-certs.sh)", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return fmt.Errorf("infra/traefik/edge-cert.pem contains no usable certificate")
	}

	edge = edgeConfig{httpsPort: port, edgePool: pool, dialAddr: net.JoinHostPort(host, port)}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	tr.DialContext = edge.dialViaEdge
	// SSE must stream, not buffer -- streamToTerminal reads events as they land.
	tr.ResponseHeaderTimeout = 30 * time.Second
	http.DefaultTransport = tr
	http.DefaultClient = &http.Client{Transport: tr}
	return nil
}

// dialViaEdge redirects the routed hostnames to the published edge port while
// leaving the TLS ServerName as the original hostname, so SNI -- and therefore
// the router rule and sniStrict -- are exercised exactly as in production.
func (c edgeConfig) dialViaEdge(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	switch host {
	case portalHost, apiHost, blobHost:
		addr = c.dialAddr
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}
