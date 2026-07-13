package main

// Executable guards for the non-negotiable security invariants in CLAUDE.md /
// plans/02-security.md (Testing Goal T3 / Part 6). Each test fails the build if
// the invariant is violated:
//
//   - mTLS on every internal hop: the printer-link listener REFUSES a client
//     with no cert or a cert from the wrong CA (the refusal is the property).
//   - Zero-knowledge cloud: no cloud code path logs encrypted_key, and the cloud
//     never decrypts anything (it only ever forwards ciphertext + metadata).
//
// The AST scanners include self-tests proving they actually catch a violation,
// so a green result means "the guard works AND the tree is clean", not "the
// guard silently matched nothing".

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- mTLS refusal ----------

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

func (ca *testCA) issue(t *testing.T, cn string, server bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		tmpl.DNSNames = []string{"localhost"}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// TestInvariant_InternalListenerRequiresClientCert drives the REAL production
// tls.Config (internalTLSConfig) and asserts the refusal semantics. If someone
// regresses ClientAuth to tls.NoClientCert, case 1 stops failing and this test
// goes red.
func TestInvariant_InternalListenerRequiresClientCert(t *testing.T) {
	ca := newTestCA(t, "automail-internal-CA")
	rogue := newTestCA(t, "rogue-CA")
	serverCert := ca.issue(t, "cloud-server", true)
	validClient := ca.issue(t, "printer", false)
	rogueClient := rogue.issue(t, "evil-printer", false)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = internalTLSConfig(ca.pool) // the actual server-side policy under test
	srv.TLS.Certificates = []tls.Certificate{serverCert}
	srv.StartTLS()
	defer srv.Close()

	// Client trusts the server cert, so only the CLIENT-auth outcome varies.
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	dial := func(clientCerts []tls.Certificate) (int, error) {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:      roots,
			Certificates: clientCerts,
			MinVersion:   tls.VersionTLS12,
		}}}
		resp, err := c.Get(srv.URL)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}

	if _, err := dial(nil); err == nil {
		t.Fatal("certless client was ACCEPTED — mTLS not enforced (want handshake failure)")
	}
	if _, err := dial([]tls.Certificate{rogueClient}); err == nil {
		t.Fatal("wrong-CA client was ACCEPTED — mTLS not enforced")
	}
	if code, err := dial([]tls.Certificate{validClient}); err != nil || code != http.StatusOK {
		t.Fatalf("valid internal-CA client rejected: code=%d err=%v", code, err)
	}
}

// ---------- zero-knowledge AST scanners ----------

var logFuncNames = map[string]bool{
	"Print": true, "Printf": true, "Println": true,
	"Fatal": true, "Fatalf": true, "Fatalln": true,
	"Panic": true, "Panicf": true, "Panicln": true,
	"Error": true, "Errorf": true, "Warn": true, "Warnf": true,
	"Info": true, "Infof": true, "Debug": true, "Debugf": true,
}

// referencesEncryptedKey reports whether an expression tree reads an
// encrypted-key VALUE (an identifier or a struct field like x.EncryptedKey).
// Plain string literals such as "encrypted_key must be base64" are descriptive
// text, not the value, and are deliberately not matched.
func referencesEncryptedKey(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		switch e := x.(type) {
		case *ast.Ident:
			if strings.Contains(strings.ToLower(e.Name), "encryptedkey") {
				found = true
			}
		case *ast.SelectorExpr:
			if strings.Contains(e.Sel.Name, "EncryptedKey") {
				found = true
			}
		}
		return !found
	})
	return found
}

// scanFileViolations returns a slice of "file:line: kind" for two invariants in
// one AST file: (a) a logging call that passes an encrypted-key value, and
// (b) any call to a Decrypt* function (the cloud must never decrypt).
func scanFileViolations(fset *token.FileSet, file *ast.File) []string {
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// Callee name whether it's a bare call (Decrypt(x)) or a selector
		// (log.Printf / key.DecryptAESKey / rsa.DecryptOAEP).
		var name string
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		case *ast.Ident:
			name = fn.Name
		default:
			return true
		}
		pos := fset.Position(call.Pos())
		// (a) logging the key value.
		if logFuncNames[name] {
			for _, arg := range call.Args {
				if referencesEncryptedKey(arg) {
					out = append(out, pos.String()+": logs encrypted_key value")
					break
				}
			}
		}
		// (b) cloud calling any decryption routine.
		if strings.HasPrefix(name, "Decrypt") {
			out = append(out, pos.String()+": calls "+name+" (cloud must never decrypt)")
		}
		return true
	})
	return out
}

// scanTree walks non-test .go files under dir and collects violations.
func scanTree(t *testing.T, dir string) []string {
	t.Helper()
	var all []string
	fset := token.NewFileSet()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		all = append(all, scanFileViolations(fset, f)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// TestInvariant_ZeroKnowledgeCloud asserts the whole cloud tree neither logs
// encrypted_key nor decrypts anything.
func TestInvariant_ZeroKnowledgeCloud(t *testing.T) {
	if v := scanTree(t, "."); len(v) > 0 {
		t.Fatalf("zero-knowledge invariant violated:\n  %s", strings.Join(v, "\n  "))
	}
}

// TestInvariant_ScannerCatchesViolations is the guard's own guard: it proves the
// scanner flags both a logged key and a Decrypt* call, so a clean run of the
// test above means the tree is clean, not that the detector is broken.
func TestInvariant_ScannerCatchesViolations(t *testing.T) {
	const bad = `package p
import "log"
func leak(encryptedKey []byte) {
	log.Printf("key=%x", encryptedKey)   // must be flagged (a)
	DecryptAESKey(encryptedKey)          // must be flagged (b)
}
func DecryptAESKey(b []byte) {}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "bad.go", bad, 0)
	if err != nil {
		t.Fatal(err)
	}
	v := scanFileViolations(fset, f)
	if len(v) != 2 {
		t.Fatalf("scanner should flag exactly the 2 planted violations, got %d:\n  %s",
			len(v), strings.Join(v, "\n  "))
	}
}
