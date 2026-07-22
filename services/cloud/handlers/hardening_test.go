package handlers

// Nasty-row edge cases for opaque-token hashing (Testing Goal T4 / Part 1):
// determinism, a URL-safe fixed-width digest for arbitrary input, and that
// newOpaqueToken's returned hash always matches hashToken of its raw token.
// (These cover the SHAPE of the digest; tokens_test.go pins its VALUE.)

import (
	"strings"
	"testing"
)

func TestHashToken_NastyRows(t *testing.T) {
	inputs := []string{
		"",                        // empty
		"a",                       // single byte
		strings.Repeat("x", 4096), // very long
		"emoji-🔐-and-ünïcode",     // multibyte
		"with\x00null\x00bytes",   // embedded NULs
	}
	for _, in := range inputs {
		h := hashToken(in)
		// SHA-256 -> RawURLEncoding is always 43 chars, URL-safe, no padding.
		if len(h) != 43 {
			t.Fatalf("hash of %q is %d chars, want 43", in, len(h))
		}
		if strings.ContainsAny(h, "+/=") {
			t.Fatalf("hash %q is not URL-safe base64", h)
		}
		if h != hashToken(in) {
			t.Fatalf("hashToken not deterministic for %q", in)
		}
	}
	// Distinct inputs must not collide at this trivial level.
	if hashToken("token-a") == hashToken("token-b") {
		t.Fatal("distinct tokens hashed to the same value")
	}
}

func TestNewOpaqueToken_HashMatchesRaw(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		raw, hash, err := newOpaqueToken()
		if err != nil {
			t.Fatal(err)
		}
		if hash != hashToken(raw) {
			t.Fatal("newOpaqueToken hash does not match hashToken(raw)")
		}
		if seen[raw] {
			t.Fatalf("newOpaqueToken produced a duplicate raw token: %q", raw)
		}
		seen[raw] = true
	}
}
