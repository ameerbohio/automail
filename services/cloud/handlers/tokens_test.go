package handlers

// Guards for the two things in the token/session path that are PERSISTED or
// leave the process, and are therefore not free to change:
//
//   - the stored token digest (jobs.guest_token_hash, refresh_tokens.token_hash)
//   - the refresh cookie's exact Set-Cookie rendering
//
// The existing edge tables (hardening_test.go) cover length, URL-safety,
// determinism and the create/verify round trip -- all of which still pass if
// someone salts the input or swaps the encoding, because they never compare
// against a known value. These pin the values.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The expected digests were computed OUTSIDE Go, with
//
//	printf '%s' <input> | sha256sum | cut -d' ' -f1 | xxd -r -p \
//	  | base64 | tr '+/' '-_' | tr -d '='
//
// so this compares the implementation against an independent derivation rather
// than against itself.
//
// A failure here means the stored-digest format changed. That invalidates every
// guest token and every refresh token already in the database -- it is a
// migration, not a refactor. Do not "fix" the test by updating the literal.
func TestHashToken_PinnedDigest(t *testing.T) {
	cases := []struct{ in, want string }{
		{"automail-token-hash-guard-vector", "Km_bwemrdty7q5wxs2iNmYSDCcVpqRSUEZaJaIMEaoA"},
		{"", "47DEQpj8HBSa-_TImW-5JCeuQeRkm5NMpJWZG3hSuFU"},
	}
	for _, tc := range cases {
		if got := hashToken(tc.in); got != tc.want {
			t.Errorf("hashToken(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Both token kinds must hash through the same definition. Before this was one
// function it was four copies, and a divergence between the Refresh side and
// the Logout side would have meant logout silently not revoking.
func TestHashToken_IsTheOnlyDefinition(t *testing.T) {
	raw, hash, err := newOpaqueToken()
	if err != nil {
		t.Fatalf("newOpaqueToken: %v", err)
	}
	if hash != hashToken(raw) {
		t.Fatalf("newOpaqueToken hash %q != hashToken(raw) %q", hash, hashToken(raw))
	}
}

// The refresh cookie's Name and Path are a CROSS-LANGUAGE contract: the portal
// rewrites Path=/auth/refresh with a regex (services/portal/lib/proxy.ts) and
// gates account pages on the cookie name (services/portal/middleware.ts).
// Changing either here silently breaks every session's survival across a page
// reload, with no error on either side. These pin both renderings byte for
// byte -- the set and the clear must agree on Name and Path or the browser
// will not delete the cookie.
func TestRefreshCookie_ExactRendering(t *testing.T) {
	expires := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	t.Run("set", func(t *testing.T) {
		rec := httptest.NewRecorder()
		setRefreshCookie(rec, "the-raw-token", expires)
		got := rec.Header().Get("Set-Cookie")
		want := "refresh_token=the-raw-token; Path=/auth/refresh; " +
			"Expires=Wed, 29 Jul 2026 12:00:00 GMT; HttpOnly; Secure; SameSite=Strict"
		if got != want {
			t.Errorf("Set-Cookie =\n  %q\nwant\n  %q", got, want)
		}
	})

	t.Run("clear", func(t *testing.T) {
		rec := httptest.NewRecorder()
		clearRefreshCookie(rec)
		got := rec.Header().Get("Set-Cookie")
		want := "refresh_token=; Path=/auth/refresh; Max-Age=0; HttpOnly; Secure; SameSite=Strict"
		if got != want {
			t.Errorf("Set-Cookie =\n  %q\nwant\n  %q", got, want)
		}
	})

	t.Run("set and clear agree on name and path", func(t *testing.T) {
		// The browser only deletes a cookie when these match.
		set := httptest.NewRecorder()
		setRefreshCookie(set, "x", expires)
		clear := httptest.NewRecorder()
		clearRefreshCookie(clear)

		name := func(rec *httptest.ResponseRecorder) (string, string) {
			t.Helper()
			res := http.Response{Header: rec.Header()}
			cs := res.Cookies()
			if len(cs) != 1 {
				t.Fatalf("expected exactly one cookie, got %d", len(cs))
			}
			return cs[0].Name, cs[0].Path
		}
		sn, sp := name(set)
		cn, cp := name(clear)
		if sn != cn || sp != cp {
			t.Fatalf("set (%s, %s) and clear (%s, %s) disagree -- the browser will not delete the cookie",
				sn, sp, cn, cp)
		}
	})
}
