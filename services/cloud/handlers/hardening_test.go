package handlers

// Nasty-row edge cases for guest-token hashing (Testing Goal T4 / Part 1):
// determinism, a URL-safe fixed-width digest for arbitrary input, and that
// newGuestToken's returned hash always matches hashGuestToken of its raw token.

import (
	"strings"
	"testing"
)

func TestHashGuestToken_NastyRows(t *testing.T) {
	inputs := []string{
		"",                        // empty
		"a",                       // single byte
		strings.Repeat("x", 4096), // very long
		"emoji-🔐-and-ünïcode",     // multibyte
		"with\x00null\x00bytes",   // embedded NULs
	}
	for _, in := range inputs {
		h := hashGuestToken(in)
		// SHA-256 -> RawURLEncoding is always 43 chars, URL-safe, no padding.
		if len(h) != 43 {
			t.Fatalf("hash of %q is %d chars, want 43", in, len(h))
		}
		if strings.ContainsAny(h, "+/=") {
			t.Fatalf("hash %q is not URL-safe base64", h)
		}
		if h != hashGuestToken(in) {
			t.Fatalf("hashGuestToken not deterministic for %q", in)
		}
	}
	// Distinct inputs must not collide at this trivial level.
	if hashGuestToken("token-a") == hashGuestToken("token-b") {
		t.Fatal("distinct tokens hashed to the same value")
	}
}

func TestNewGuestToken_HashMatchesRaw(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		raw, hash, err := newGuestToken()
		if err != nil {
			t.Fatal(err)
		}
		if hash != hashGuestToken(raw) {
			t.Fatal("newGuestToken hash does not match hashGuestToken(raw)")
		}
		if seen[raw] {
			t.Fatalf("newGuestToken produced a duplicate raw token: %q", raw)
		}
		seen[raw] = true
	}
}
