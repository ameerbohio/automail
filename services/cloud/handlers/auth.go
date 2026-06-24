package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"automail/cloud/db"
	"automail/cloud/jwtutil"

	"golang.org/x/crypto/bcrypt"
)

const refreshTokenTTL = 7 * 24 * time.Hour

func newRefreshToken() (raw string, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(raw))
	hash = base64.RawURLEncoding.EncodeToString(sum[:])
	return raw, hash, nil
}

func setRefreshCookie(w http.ResponseWriter, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     "refresh_token",
		Value:    token,
		Path:     "/auth/refresh",
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// Login handles POST /auth/login (plans/09-api-contracts.md). bcrypt
// verifies the password; on success it issues a short-lived RS256 access
// token and sets a rotating refresh token as an HttpOnly cookie.
func (s *Server) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid request body", "INVALID_BODY")
		return
	}

	sender, err := s.Queries.GetSenderByEmail(r.Context(), db.GetSenderByEmailParams{
		AppKey: s.AppKey,
		Email:  req.Email,
	})
	if errors.Is(err, sql.ErrNoRows) {
		WriteError(w, http.StatusUnauthorized, "invalid credentials", "UNAUTHORIZED")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "login failed", "INTERNAL")
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(sender.PasswordHash), []byte(req.Password)); err != nil {
		WriteError(w, http.StatusUnauthorized, "invalid credentials", "UNAUTHORIZED")
		return
	}

	access, err := jwtutil.IssueAccessToken(s.JWTPriv, sender.ID, sender.Role)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not issue token", "INTERNAL")
		return
	}

	rawRefresh, refreshHash, err := newRefreshToken()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not issue refresh token", "INTERNAL")
		return
	}
	expiresAt := time.Now().Add(refreshTokenTTL)
	if err := s.Queries.InsertRefreshToken(r.Context(), db.InsertRefreshTokenParams{
		SenderID:  sender.ID,
		TokenHash: refreshHash,
		ExpiresAt: expiresAt,
	}); err != nil {
		WriteError(w, http.StatusInternalServerError, "could not store refresh token", "INTERNAL")
		return
	}

	setRefreshCookie(w, rawRefresh, expiresAt)
	WriteJSON(w, http.StatusOK, tokenResponse{
		AccessToken: access,
		ExpiresIn:   int(jwtutil.AccessTokenTTL.Seconds()),
	})
}

// Refresh handles POST /auth/refresh. The refresh token is single-use: a
// successful call immediately revokes it and issues a new one
// (plans/02-security.md "Rotation").
func (s *Server) Refresh(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("refresh_token")
	if err != nil {
		WriteError(w, http.StatusUnauthorized, "missing refresh token", "UNAUTHORIZED")
		return
	}
	sum := sha256.Sum256([]byte(cookie.Value))
	hash := base64.RawURLEncoding.EncodeToString(sum[:])

	active, err := s.Queries.GetActiveRefreshToken(r.Context(), hash)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && active.ExpiresAt.Before(time.Now())) {
		WriteError(w, http.StatusUnauthorized, "expired or revoked token", "UNAUTHORIZED")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "refresh failed", "INTERNAL")
		return
	}

	if err := s.Queries.RevokeRefreshTokenByHash(r.Context(), hash); err != nil {
		WriteError(w, http.StatusInternalServerError, "could not revoke token", "INTERNAL")
		return
	}

	sender, err := s.Queries.GetSenderByID(r.Context(), active.SenderID)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "lookup failed", "INTERNAL")
		return
	}

	access, err := jwtutil.IssueAccessToken(s.JWTPriv, sender.ID, sender.Role)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not issue token", "INTERNAL")
		return
	}

	rawRefresh, refreshHash, err := newRefreshToken()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not issue refresh token", "INTERNAL")
		return
	}
	expiresAt := time.Now().Add(refreshTokenTTL)
	if err := s.Queries.InsertRefreshToken(r.Context(), db.InsertRefreshTokenParams{
		SenderID:  sender.ID,
		TokenHash: refreshHash,
		ExpiresAt: expiresAt,
	}); err != nil {
		WriteError(w, http.StatusInternalServerError, "could not store refresh token", "INTERNAL")
		return
	}

	setRefreshCookie(w, rawRefresh, expiresAt)
	WriteJSON(w, http.StatusOK, tokenResponse{
		AccessToken: access,
		ExpiresIn:   int(jwtutil.AccessTokenTTL.Seconds()),
	})
}

// Logout handles POST /auth/logout. Revokes the refresh token in the DB
// and clears the cookie; the access token is left to expire naturally --
// it's short-lived enough not to need a blocklist (plans/02-security.md).
func (s *Server) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("refresh_token"); err == nil {
		sum := sha256.Sum256([]byte(cookie.Value))
		hash := base64.RawURLEncoding.EncodeToString(sum[:])
		_ = s.Queries.RevokeRefreshTokenByHash(r.Context(), hash)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "refresh_token",
		Value:    "",
		Path:     "/auth/refresh",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}
