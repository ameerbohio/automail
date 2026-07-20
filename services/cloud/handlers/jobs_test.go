package handlers

import (
	"testing"

	"github.com/minio/minio-go/v7"
)

// TestUploadPresignerSelection locks the split-endpoint rule: the browser-facing
// upload URL is signed by the dedicated public-endpoint client when one is
// configured, and falls back to the internal client otherwise. Server-side blob
// ops (BlobExists, RemoveBlob) always use s.Minio regardless.
func TestUploadPresignerSelection(t *testing.T) {
	internal := &minio.Client{}
	public := &minio.Client{}

	if got := (&Server{Minio: internal}).uploadPresigner(); got != internal {
		t.Fatalf("with no public endpoint, presigner should be the internal client")
	}
	if got := (&Server{Minio: internal, UploadPresigner: public}).uploadPresigner(); got != public {
		t.Fatalf("with a public endpoint, presigner should be the public client")
	}
}

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
