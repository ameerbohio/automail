package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"automail/cloud/db"
	"automail/cloud/jwtutil"

	"github.com/google/uuid"
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

// issueSession mints an access token and a fresh rotating refresh cookie for an
// authenticated sender. It is the shared tail of Login, Refresh, and Register
// (auto-login) so the three paths can never drift on token issuance.
func (s *Server) issueSession(w http.ResponseWriter, ctx context.Context, senderID uuid.UUID, role string) (tokenResponse, error) {
	access, err := jwtutil.IssueAccessToken(s.JWTPriv, senderID, role)
	if err != nil {
		return tokenResponse{}, err
	}
	rawRefresh, refreshHash, err := newRefreshToken()
	if err != nil {
		return tokenResponse{}, err
	}
	expiresAt := time.Now().Add(refreshTokenTTL)
	if err := s.Queries.InsertRefreshToken(ctx, db.InsertRefreshTokenParams{
		SenderID:  senderID,
		TokenHash: refreshHash,
		ExpiresAt: expiresAt,
	}); err != nil {
		return tokenResponse{}, err
	}
	setRefreshCookie(w, rawRefresh, expiresAt)
	return tokenResponse{
		AccessToken: access,
		ExpiresIn:   int(jwtutil.AccessTokenTTL.Seconds()),
	}, nil
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
		// Same lower-case normalization Register applies at signup, so login is
		// case-insensitive against the stored address.
		Email: strings.ToLower(req.Email),
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

	resp, err := s.issueSession(w, r.Context(), sender.ID, sender.Role)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not issue token", "INTERNAL")
		return
	}
	WriteJSON(w, http.StatusOK, resp)
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

	resp, err := s.issueSession(w, r.Context(), sender.ID, sender.Role)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not issue token", "INTERNAL")
		return
	}
	WriteJSON(w, http.StatusOK, resp)
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

const minPasswordLen = 8

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Register handles POST /auth/register: open self-service signup
// (plans/09-api-contracts.md). Anyone can create a sender account to send mail
// and see their own history -- no invite, no admin approval, no email
// verification. On success it auto-logs-in (issues the same token pair as
// Login) so the portal lands straight in the authenticated flow. role is fixed
// to 'sender' by InsertSender -- admin is never self-assignable here.
func (s *Server) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid request body", "INVALID_BODY")
		return
	}

	addr, err := mail.ParseAddress(req.Email)
	if err != nil {
		WriteError(w, http.StatusUnprocessableEntity, "invalid email address", "VALIDATION")
		return
	}
	// Normalize to lower-case so the duplicate pre-check and later logins are
	// case-insensitive (providers treat addresses that way in practice). Login
	// applies the same normalization before its lookup.
	email := strings.ToLower(addr.Address)
	if len([]rune(req.Password)) < minPasswordLen {
		WriteError(w, http.StatusUnprocessableEntity, "password must be at least 8 characters", "VALIDATION")
		return
	}

	// Duplicate pre-check. email_enc is non-deterministically encrypted, so
	// uniqueness cannot be a DB constraint (see InsertSender) -- we scan by
	// decrypt-compare instead. A narrow race (two concurrent signups with the
	// same email) can still slip two rows through at prototype scale; login
	// would then match the first. Acceptable here; a deterministic blind-index
	// column is the real fix (noted in docs/study).
	if _, err := s.Queries.GetSenderByEmail(r.Context(), db.GetSenderByEmailParams{
		AppKey: s.AppKey,
		Email:  email,
	}); err == nil {
		WriteError(w, http.StatusConflict, "email already registered", "EMAIL_TAKEN")
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		WriteError(w, http.StatusInternalServerError, "registration failed", "INTERNAL")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not hash password", "INTERNAL")
		return
	}

	// Least-friction signup collects only email + password; derive a display
	// name from the email local-part so name_enc (NOT NULL) has a sensible
	// value without asking for another field.
	name := email
	if at := strings.IndexByte(email, '@'); at > 0 {
		name = email[:at]
	}

	sender, err := s.Queries.InsertSender(r.Context(), db.InsertSenderParams{
		AppKey:       s.AppKey,
		Email:        email,
		Name:         name,
		PasswordHash: string(hash),
	})
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not create account", "INTERNAL")
		return
	}

	resp, err := s.issueSession(w, r.Context(), sender.ID, sender.Role)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not issue token", "INTERNAL")
		return
	}
	WriteJSON(w, http.StatusCreated, resp)
}
