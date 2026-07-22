//go:build smoke

// Deployment-parity smoke (Goal T12). Runs the Part 5 job lifecycle against the
// PRODUCTION-PROFILE stack -- base docker-compose.yml, reached only through the
// Traefik HTTPS edge on the routed hostnames -- rather than against an override
// that publishes services on direct host ports.
//
// Why a separate suite instead of reusing fullstack_test.go: the two prove
// different things. T8 proves the *distributed* seams (two cloud nodes, Redis
// fan-in/fan-out) using a topology no deploy actually runs. This proves the
// *deployment* seams that only exist when the edge is in the path -- SNI and the
// edge certificate, the secure-headers CSP, and the pre-signed upload URL
// naming a host something outside the Docker network can resolve. Every
// first-deploy bug this project has actually hit lived in exactly that gap, and
// no other suite covers it (T7 and T8 both bypass Traefik by design).
//
// Run it with `make deploy-smoke`, never bare `go test`: scripts/deploy/smoke.sh
// brings the stack up, seeds the fixture, and sets the env knobs below.
package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The production hostnames Traefik routes. These are the names the browser uses
// and the names the edge certificate must carry SANs for; nothing here may fall
// back to localhost or a published port, or the suite stops testing the edge.
const (
	portalHost = "automail.local"
	apiHost    = "api.automail.local"
	blobHost   = "blob.automail.local"
)

// smokeConfig is resolved once in TestMain from the env scripts/deploy/smoke.sh
// sets.
type smokeConfig struct {
	httpsPort string         // host port Traefik's websecure entrypoint is published on
	edgePool  *x509.CertPool // trusts the self-signed edge cert (browsers get a warning, not a failure)
	dialAddr  string         // where the routed hostnames actually live
}

var smoke smokeConfig

// TestMain wires the whole suite to the edge. It installs a DefaultTransport
// that (a) trusts the self-signed edge cert and (b) maps every routed hostname to
// the published Traefik port -- the programmatic equivalent of curl's
// `--resolve`, and of the hosts-file entry a real deploy needs
// (docs/deploy-checklist.md). Doing it at the transport layer means the shared
// harness helpers drive real `https://api.automail.local/...` URLs with no
// knowledge that the edge is involved, so what they exercise is what a browser
// would.
//
// InsecureSkipVerify is deliberately NOT used: the cert is self-signed, not
// invalid, and pinning it here is what makes "the edge serves the cert we
// generated, for the SNI we asked for" an assertion rather than an assumption.
func TestMain(m *testing.M) {
	root := os.Getenv("E2E_REPO_ROOT")
	if root == "" {
		fmt.Fprintln(os.Stderr, "E2E_REPO_ROOT is unset -- run via `make deploy-smoke`, not bare `go test`")
		os.Exit(1)
	}
	port := os.Getenv("SMOKE_HTTPS_PORT")
	if port == "" {
		port = "443"
	}
	host := os.Getenv("SMOKE_EDGE_HOST")
	if host == "" {
		host = "127.0.0.1"
	}

	pem, err := os.ReadFile(filepath.Join(root, "infra", "traefik", "edge-cert.pem"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "read edge cert: %v\n(run ./infra/certs/gen-edge-certs.sh)\n", err)
		os.Exit(1)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		fmt.Fprintln(os.Stderr, "infra/traefik/edge-cert.pem contains no usable certificate")
		os.Exit(1)
	}

	smoke = smokeConfig{httpsPort: port, edgePool: pool, dialAddr: net.JoinHostPort(host, port)}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	tr.DialContext = smoke.dialViaEdge
	// SSE must stream, not buffer -- streamToTerminal reads events as they land.
	tr.ResponseHeaderTimeout = 30 * time.Second
	http.DefaultTransport = tr
	http.DefaultClient = &http.Client{Transport: tr}

	os.Exit(m.Run())
}

// dialViaEdge redirects the routed hostnames to the published Traefik port while
// leaving the TLS ServerName as the original hostname, so SNI -- and therefore
// the router rule and sniStrict -- are exercised exactly as in production.
func (c smokeConfig) dialViaEdge(ctx context.Context, network, addr string) (net.Conn, error) {
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

// dialSNI opens a raw TLS connection to the edge announcing serverName, and
// returns the handshake error (nil on success) plus the leaf certificate served.
func dialSNI(t *testing.T, serverName string) (*x509.Certificate, error) {
	t.Helper()
	conn, err := tls.Dial("tcp", smoke.dialAddr, &tls.Config{
		ServerName: serverName,
		RootCAs:    smoke.edgePool,
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	chain := conn.ConnectionState().PeerCertificates
	if len(chain) == 0 {
		t.Fatalf("edge presented no certificate for SNI %q", serverName)
	}
	return chain[0], nil
}

// TestEdgeTLS_RoutedHostnamesHandshake is the direct regression guard for the
// ERR_SSL_UNRECOGNIZED_NAME_ALERT first-deploy bug (c8716b1): with sniStrict on
// and no cert covering a routed hostname, Traefik hard-rejects the handshake
// before any HTTP request happens. Every hostname the stack routes must be
// covered, which is why blob.automail.local is in this list -- adding a router
// without adding its SAN reintroduces the exact same outage for uploads.
func TestEdgeTLS_RoutedHostnamesHandshake(t *testing.T) {
	for _, host := range []string{portalHost, apiHost, blobHost} {
		leaf, err := dialSNI(t, host)
		if err != nil {
			t.Fatalf("TLS handshake to the edge with SNI %q failed: %v\n"+
				"the edge cert must carry a SAN for every routed hostname "+
				"(regenerate with ./infra/certs/gen-edge-certs.sh)", host, err)
		}
		if err := leaf.VerifyHostname(host); err != nil {
			t.Fatalf("edge cert does not cover %q: %v (SANs: %v)", host, err, leaf.DNSNames)
		}
		t.Logf("SNI %-24s -> cert CN=%q SANs=%v", host, leaf.Subject.CommonName, leaf.DNSNames)
	}
}

// TestEdgeTLS_UnknownSNIRejected pins the other half of the fix: the bug was
// resolved by supplying the missing certificate, NOT by relaxing sniStrict. If
// someone "fixes" a future SNI failure by turning sniStrict off, the handshake
// above would still pass and only this test would notice.
//
// It must skip verification to mean anything. With sniStrict off, Traefik falls
// back to tls.stores.default.defaultCertificate -- which IS configured -- so a
// verifying client would still error, just with a hostname mismatch instead of a
// server alert. "Some error occurred" would then pass in both worlds. Skipping
// verification makes the assertion specifically about what the SERVER did.
func TestEdgeTLS_UnknownSNIRejected(t *testing.T) {
	conn, err := tls.Dial("tcp", smoke.dialAddr, &tls.Config{
		ServerName:         "not-a-routed-host.invalid",
		InsecureSkipVerify: true, //nolint:gosec // asserting the server's rejection, not trusting the peer
		MinVersion:         tls.VersionTLS12,
	})
	if err == nil {
		conn.Close()
		t.Fatal("edge completed a TLS handshake for an unrouted SNI -- sniStrict is no longer enforced " +
			"(infra/traefik/dynamic.yml tls.options.default.sniStrict)")
	}
	// Traefik signals sniStrict with the TLS `unrecognized_name` alert (RFC 6066),
	// which Go surfaces as `remote error: tls: unrecognized name`. Matching on that
	// text is what distinguishes a server-side rejection from a client-side
	// certificate-name mismatch (`x509: certificate is valid for ...`) -- the error
	// we would get if sniStrict were off and Traefik fell back to its default cert.
	// Type-matching does not work here: remote alerts arrive wrapped in a
	// *net.OpError around an unexported alert type, not as tls.AlertError.
	if !strings.Contains(err.Error(), "unrecognized name") {
		t.Fatalf("handshake failed, but not with the server's unrecognized_name alert: %v\n"+
			"that suggests the connection died for some other reason and this guard is no longer "+
			"proving sniStrict is on", err)
	}
	t.Logf("unrouted SNI rejected by the server: %v", err)
}

// cspDirective pulls one directive's value out of a CSP header, falling back to
// default-src the way a browser does.
func cspDirective(csp, name string) (string, bool) {
	var fallback string
	var found bool
	for _, d := range strings.Split(csp, ";") {
		fields := strings.Fields(strings.TrimSpace(d))
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case name:
			return strings.Join(fields[1:], " "), true
		case "default-src":
			fallback, found = strings.Join(fields[1:], " "), true
		}
	}
	return fallback, found
}

// TestEdgeServesPortalWithWorkableCSP checks that the production security headers
// leave the product able to function. Both directives asserted here were broken
// on the first production-profile run, and both fail in ways an HTTP status can
// never show:
//
//   - script-src: Next.js App Router streams its RSC payload in inline <script>
//     blocks. Under `default-src 'self'` they are all blocked -- the SSR HTML
//     still returns 200 and looks correct, but nothing hydrates and no request is
//     ever made. A green "portal returns 200" proves nothing here.
//   - connect-src: the ciphertext PUT to object storage is cross-origin, so
//     `default-src 'self'` alone makes the browser refuse it.
//
// IMPORTANT about what this does and does not prove: a Go client neither executes
// scripts nor enforces CSP, so this asserts the *policy*, not the browser's
// behaviour under it. That is why the assertions are on parsed directives rather
// than on a status code. Catching a genuinely novel CSP break still needs a real
// browser through the edge -- see docs/study/17.
func TestEdgeServesPortalWithWorkableCSP(t *testing.T) {
	resp, err := http.Get("https://" + portalHost + "/")
	if err != nil {
		t.Fatalf("GET the portal through the edge: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("portal through the edge: status %d, want 200", resp.StatusCode)
	}

	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy header -- the secure-headers middleware is not applied at the edge")
	}

	scriptSrc, ok := cspDirective(csp, "script-src")
	if !ok || !strings.Contains(scriptSrc, "'unsafe-inline'") {
		t.Fatalf("CSP script-src is %q; without 'unsafe-inline' (or a nonce scheme) the browser blocks "+
			"Next.js's inline RSC scripts, so the portal renders but never hydrates.\nfull CSP: %s", scriptSrc, csp)
	}

	connectSrc, ok := cspDirective(csp, "connect-src")
	if !ok || !strings.Contains(connectSrc, "https://"+blobHost) {
		t.Fatalf("CSP connect-src is %q; it must allow https://%s or the browser blocks the ciphertext "+
			"upload PUT.\nfull CSP: %s", connectSrc, blobHost, csp)
	}

	if hsts := resp.Header.Get("Strict-Transport-Security"); hsts == "" {
		t.Error("no Strict-Transport-Security header at the edge")
	}
	t.Logf("CSP: %s", csp)
}

// TestUploadURLIsBrowserReachable is the regression guard for the second bug
// this goal found: without MINIO_PUBLIC_ENDPOINT the cloud server signs the
// upload URL against MINIO_ENDPOINT (`minio:9000`) -- a Docker-internal name over
// plaintext HTTP. The guest flow then fails at its first step on every fresh
// deploy, and nothing in the stack logs an error, because from the server's side
// handing out the URL succeeded.
//
// Asserted separately from the lifecycle test below so a failure says "the URL
// is wrong" rather than surfacing as an opaque upload timeout.
func TestUploadURLIsBrowserReachable(t *testing.T) {
	var recips []recipient
	getJSON(t, "https://"+apiHost+"/recipients?q=Testmann", &recips)
	if len(recips) == 0 {
		t.Fatal("no seeded recipient found (did scripts/e2e/seed.sh run?)")
	}

	var up uploadURLResp
	if code := postJSON(t, "https://"+apiHost+"/jobs/upload-url",
		map[string]string{"recipient_id": recips[0].RecipientID, "filename": "letter.pdf"}, &up); code != http.StatusOK {
		t.Fatalf("POST /jobs/upload-url: status %d", code)
	}

	if !strings.HasPrefix(up.UploadURL, "https://"+blobHost+"/") {
		t.Fatalf("pre-signed upload_url is %q, want an https://%s/... URL.\n"+
			"An internal endpoint here (e.g. http://minio:9000/) means no browser can complete the "+
			"upload -- set MINIO_PUBLIC_ENDPOINT/MINIO_PUBLIC_SECURE on cloud-server.",
			up.UploadURL, blobHost)
	}
	t.Logf("upload_url host is browser-reachable: %s", strings.SplitN(up.UploadURL, "?", 2)[0])

	// Reachable is necessary but not sufficient: the PUT is cross-origin
	// (automail.local -> blob.automail.local), so a real browser sends a CORS
	// preflight first and refuses the upload if object storage does not answer
	// it. This suite's own PUT below is a Go client, which never preflights --
	// so without this assertion the CORS configuration would be untested and a
	// regression would only ever be found by a human with a browser.
	req, err := http.NewRequest(http.MethodOptions, up.UploadURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://"+portalHost)
	req.Header.Set("Access-Control-Request-Method", http.MethodPut)
	// The upload sets Content-Type: application/octet-stream, which is not a
	// CORS-safelisted value, so the real browser asks about it here too.
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	pre, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("CORS preflight to %s: %v", blobHost, err)
	}
	defer pre.Body.Close()
	if pre.StatusCode >= 400 {
		t.Fatalf("CORS preflight to %s returned status %d; the browser will block the ciphertext PUT",
			blobHost, pre.StatusCode)
	}
	if h := pre.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(strings.ToLower(h), "content-type") {
		t.Fatalf("CORS preflight does not allow the content-type header (got %q); the browser will block "+
			"the ciphertext PUT, which sends Content-Type: application/octet-stream", h)
	}
	allowed := pre.Header.Get("Access-Control-Allow-Origin")
	if allowed != "*" && allowed != "https://"+portalHost {
		t.Fatalf("CORS preflight (status %d) returned Access-Control-Allow-Origin %q; the browser will "+
			"block the ciphertext PUT. Set MINIO_API_CORS_ALLOW_ORIGIN to the portal origin.",
			pre.StatusCode, allowed)
	}
	t.Logf("CORS preflight allows origin: %s", allowed)
}

// TestProductionProfile_JobReachesDelivered is the Part 5 lifecycle, driven
// entirely through the edge: resolve the recipient, encrypt exactly as the
// browser does, PUT the ciphertext to object storage over HTTPS, submit, stream
// to `delivered`, then prove no plaintext survived in /dev/shm.
//
// A green run here means a fresh clone plus the documented bring-up steps
// produce a working system on the first try -- which is the whole point of this
// goal. The one thing it cannot prove is paper coming out of the printer:
// DEV_MODE=true skips only the `lp` call (docker-compose.deploy-smoke.yml), and
// the physical step stays owner-gated on the Proxmox VM.
func TestProductionProfile_JobReachesDelivered(t *testing.T) {
	apiURL := "https://" + apiHost

	job := submitEncryptedJob(t, apiURL)
	t.Logf("job %s submitted through the edge, status %q", job.JobID, job.Status)
	if job.Status != "dispatching" {
		t.Fatalf("POST /jobs returned status %q, want \"dispatching\".\n"+
			"\"queued\" here means the job was accepted but no printer was eligible -- on a fresh deploy "+
			"the usual cause is the printer's SLOT_ID not matching the mailbox_slots.id row "+
			"(check the cloud-server log for \"reported no slot\").", job.Status)
	}

	statuses := streamToTerminal(t, apiURL, job.JobID, job.GuestToken, 90*time.Second)
	t.Logf("status trail: %v", statuses)
	if len(statuses) == 0 || statuses[len(statuses)-1] != "delivered" {
		t.Fatalf("job did not reach \"delivered\" through the production profile; trail: %v", statuses)
	}

	assertDevShmClean(t)
}

// TestGuestPathIsRateLimited pins plans/02-security.md §6 ("20 requests/min per
// IP on upload and job submission endpoints") to the path traffic actually takes.
// The browser never contacts api.automail.local -- it calls same-origin /api/*,
// which Next proxies server-side -- so a rate limit attached only to the API
// hostname is real config that throttles nobody. Asserting the limit on the
// *portal* origin is what makes the middleware meaningful.
//
// Runs last (Go orders tests within a file) so the tokens it burns cannot
// interfere with the lifecycle test above.
func TestGuestPathIsRateLimited(t *testing.T) {
	const attempts = 40
	limited := 0
	for i := 0; i < attempts; i++ {
		resp, err := http.Get("https://" + portalHost + "/api/recipients?q=Testmann")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Fatalf("%d rapid unauthenticated requests to https://%s/api/recipients were all accepted -- "+
			"guest-ratelimit@file is not applied to the origin the browser uses "+
			"(docker-compose.yml, portal-guest router)", attempts, portalHost)
	}
	t.Logf("%d/%d requests throttled -- guest rate limit is live on the browser's path", limited, attempts)
}
