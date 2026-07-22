package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// Opaque bearer tokens — the single definition, shared by the two kinds this
// server issues:
//
//	guest tracking tokens  jobs.guest_token_hash        (CreateJob / authorizeStream)
//	refresh tokens         refresh_tokens.token_hash    (issueSession / Refresh / Logout)
//
// Both follow the same rule: 32 bytes of CSPRNG entropy go to the client once,
// and only the digest is ever stored, so a stolen database dump yields no
// usable token (plans/02-security.md "Refresh token stored as a hash").
//
// This used to be four copies of the same three lines — one named
// (hashGuestToken) and three inline in auth.go's newRefreshToken, Refresh and
// Logout. A divergence between the Refresh side and the Logout side would have
// meant logout silently failing to revoke the session it claimed to revoke, and
// no test would have caught it because each exercised a single consistent path.
//
// *** The digest format is PERSISTED. *** Changing the hash or the encoding
// invalidates every guest token and refresh token already in the database. That
// is a migration with a rollout plan, not a refactor. tokens_test.go pins the
// output against an independently-derived value to make an accidental change
// fail loudly.

// hashToken maps a raw token to the value stored for it. Deliberately
// unsalted and fast: the input is 32 bytes of CSPRNG output, not a password,
// so there is nothing to brute-force and no reason to pay a KDF's cost. (Do
// not reuse this for passwords — those go through bcrypt in auth.go.)
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// newOpaqueToken returns a fresh random token and the digest to store for it.
// The caller hands `raw` to the client exactly once and persists only `hash`.
func newOpaqueToken() (raw string, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, hashToken(raw), nil
}
