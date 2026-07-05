package handlers

import "testing"

// TestGuestTokenHashRoundTrip locks the invariant that a token issued at
// job creation (newGuestToken, POST /jobs) verifies against the stored
// hash using the same helper the stream handler's ?token= check uses
// (authorizeStream -> hashGuestToken). If creation and verification ever
// hash differently, every guest tracking link breaks silently.
func TestGuestTokenHashRoundTrip(t *testing.T) {
	raw, hash, err := newGuestToken()
	if err != nil {
		t.Fatalf("newGuestToken: %v", err)
	}
	if raw == "" || hash == "" {
		t.Fatal("newGuestToken returned empty token or hash")
	}
	if got := hashGuestToken(raw); got != hash {
		t.Fatalf("hashGuestToken(raw) = %q, want the stored hash %q", got, hash)
	}
	if hashGuestToken("some-other-token") == hash {
		t.Fatal("different tokens must not hash to the same stored value")
	}
}
