package jwtutil

// Edge-case coverage for access-token verification (Testing Goal T4 / Part 1).
// jwtutil had no tests; these pin the security-relevant rejections — expiry,
// wrong signer, the RS256->HS256 algorithm-confusion forgery, and malformed
// input — not just the happy path.

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func mustKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func signRS256(t *testing.T, key *rsa.PrivateKey, claims AccessClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestParseAccessToken_ValidRoundTrip(t *testing.T) {
	key := mustKey(t)
	id := uuid.New()
	raw, err := IssueAccessToken(key, id, "admin")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := ParseAccessToken(&key.PublicKey, raw)
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if claims.Role != "admin" || claims.Subject != id.String() {
		t.Fatalf("claims mismatch: role=%q subject=%q", claims.Role, claims.Subject)
	}
}

func TestParseAccessToken_Rejections(t *testing.T) {
	key := mustKey(t)
	other := mustKey(t)
	id := uuid.New()
	base := func() AccessClaims {
		return AccessClaims{Role: "sender", RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.String(),
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-time.Minute)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
		}}
	}

	t.Run("expired", func(t *testing.T) {
		c := base()
		c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))
		if _, err := ParseAccessToken(&key.PublicKey, signRS256(t, key, c)); err == nil {
			t.Fatal("expired token accepted")
		}
	})

	t.Run("not yet valid (nbf in future)", func(t *testing.T) {
		c := base()
		c.NotBefore = jwt.NewNumericDate(time.Now().Add(time.Hour))
		if _, err := ParseAccessToken(&key.PublicKey, signRS256(t, key, c)); err == nil {
			t.Fatal("not-yet-valid token accepted")
		}
	})

	t.Run("signed by a different key", func(t *testing.T) {
		if _, err := ParseAccessToken(&key.PublicKey, signRS256(t, other, base())); err == nil {
			t.Fatal("token signed by the wrong key accepted")
		}
	})

	// The RS256->HS256 confusion attack: forge a token with HS256 using the
	// (public) RSA key bytes as the HMAC secret. WithValidMethods(["RS256"])
	// must reject it before the keyfunc ever runs.
	t.Run("algorithm confusion HS256", func(t *testing.T) {
		pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, base()).SignedString(pubDER)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseAccessToken(&key.PublicKey, forged); err == nil {
			t.Fatal("HS256-forged token accepted — algorithm confusion not prevented")
		}
	})

	t.Run("malformed", func(t *testing.T) {
		for _, raw := range []string{"", "garbage", "a.b.c", "not.a.jwt.at.all"} {
			if _, err := ParseAccessToken(&key.PublicKey, raw); err == nil {
				t.Fatalf("malformed token %q accepted", raw)
			}
		}
	})
}
