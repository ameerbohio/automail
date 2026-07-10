package main

// Phase 8 tests for open registration (POST /auth/register) and the account
// job-history read (GET /jobs -> ListMyJobs). Like the Phase 5 stream tests,
// these run the real sqlc query layer against the fake database/sql driver
// from dbfake_test.go -- no Postgres. Handlers are invoked directly with an
// httptest recorder; the auth context ListMyJobs reads is set the same way
// requireAuth would (authctx.WithSender).

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"automail/cloud/authctx"
	"automail/cloud/db"
	"automail/cloud/handlers"

	"github.com/google/uuid"
)

// newAuthTestServer builds a handlers.Server backed by the fake DB driver and
// a throwaway RS256 key -- enough for Register/ListMyJobs, which need no Redis,
// MinIO, or printer hub.
func newAuthTestServer(t *testing.T, q fakeQueryFunc) *handlers.Server {
	t.Helper()
	sqlDB := sql.OpenDB(fakeConnector{q: q})
	t.Cleanup(func() { _ = sqlDB.Close() })
	priv := newJWTKey(t)
	return &handlers.Server{
		Queries: db.New(sqlDB),
		SQLDB:   sqlDB,
		AppKey:  "test-app-key",
		JWTPriv: priv,
		JWTPub:  &priv.PublicKey,
	}
}

// registerQueries serves the three queries Register issues: the duplicate
// pre-check (GetSenderByEmail), the insert (InsertSender), and the refresh-token
// write from auto-login (InsertRefreshToken). emailTaken=true makes the
// pre-check return a row, simulating an already-registered address.
func registerQueries(emailTaken bool, newID string) fakeQueryFunc {
	return func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		switch {
		case strings.HasPrefix(query, "-- name: GetSenderByEmail"):
			cols := []string{"id", "email_enc", "password_hash", "role"}
			if !emailTaken {
				return cols, nil, nil // zero rows -> sql.ErrNoRows (fresh email)
			}
			return cols, [][]driver.Value{{newID, []byte("enc"), "$2a$existing", "sender"}}, nil
		case strings.HasPrefix(query, "-- name: InsertSender"):
			return []string{"id", "role"}, [][]driver.Value{{newID, "sender"}}, nil
		case strings.HasPrefix(query, "-- name: InsertRefreshToken"):
			return nil, nil, nil // :exec
		default:
			return nil, nil, fmt.Errorf("unexpected query: %.60s", query)
		}
	}
}

func postRegister(t *testing.T, srv *handlers.Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Register(rec, req)
	return rec
}

func TestRegister_OpenSignupAutoLogin(t *testing.T) {
	id := uuid.NewString()
	srv := newAuthTestServer(t, registerQueries(false, id))

	rec := postRegister(t, srv, `{"email":"newuser@example.com","password":"hunter2pass"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AccessToken == "" {
		t.Error("expected an access_token (auto-login), got none")
	}
	if resp.ExpiresIn <= 0 {
		t.Errorf("expires_in = %d, want > 0", resp.ExpiresIn)
	}
	// Auto-login must also set the rotating refresh cookie.
	var gotRefresh bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == "refresh_token" {
			gotRefresh = true
			if !c.HttpOnly {
				t.Error("refresh_token cookie must be HttpOnly")
			}
		}
	}
	if !gotRefresh {
		t.Error("expected a refresh_token cookie to be set")
	}
}

func TestRegister_DuplicateEmailConflict(t *testing.T) {
	srv := newAuthTestServer(t, registerQueries(true, uuid.NewString()))

	rec := postRegister(t, srv, `{"email":"taken@example.com","password":"hunter2pass"}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "EMAIL_TAKEN") {
		t.Errorf("expected EMAIL_TAKEN code, got %s", rec.Body.String())
	}
}

func TestRegister_Validation(t *testing.T) {
	// These reject before any DB call; the query func should never be hit.
	unreached := func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		return nil, nil, fmt.Errorf("query should not run for invalid input: %.40s", query)
	}
	cases := []struct{ name, body string }{
		{"weak password", `{"email":"ok@example.com","password":"short"}`},
		{"invalid email", `{"email":"not-an-email","password":"hunter2pass"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAuthTestServer(t, unreached)
			rec := postRegister(t, srv, tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "VALIDATION") {
				t.Errorf("expected VALIDATION code, got %s", rec.Body.String())
			}
		})
	}
}

func TestRegister_NormalizesEmailToLowercase(t *testing.T) {
	var insertedEmail string
	q := func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		switch {
		case strings.HasPrefix(query, "-- name: GetSenderByEmail"):
			return []string{"id", "email_enc", "password_hash", "role"}, nil, nil
		case strings.HasPrefix(query, "-- name: InsertSender"):
			// InsertSender arg order is email, app_key, name, password_hash.
			if s, ok := args[0].Value.(string); ok {
				insertedEmail = s
			}
			return []string{"id", "role"}, [][]driver.Value{{uuid.NewString(), "sender"}}, nil
		case strings.HasPrefix(query, "-- name: InsertRefreshToken"):
			return nil, nil, nil
		default:
			return nil, nil, fmt.Errorf("unexpected query: %.60s", query)
		}
	}
	srv := newAuthTestServer(t, q)

	rec := postRegister(t, srv, `{"email":"Foo@Example.COM","password":"hunter2pass"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if insertedEmail != "foo@example.com" {
		t.Errorf("stored email = %q, want lowercased foo@example.com", insertedEmail)
	}
}

// listJobsQueries serves GetJobsBySender from a static set of rows.
func listJobsQueries(rows [][]driver.Value) fakeQueryFunc {
	return func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		if !strings.HasPrefix(query, "-- name: GetJobsBySender") {
			return nil, nil, fmt.Errorf("unexpected query: %.60s", query)
		}
		return []string{"id", "status", "page_count", "created_at", "delivered_at"}, rows, nil
	}
}

func TestListMyJobs_ReturnsSenderHistoryMetadataOnly(t *testing.T) {
	delivered := time.Date(2026, 7, 8, 10, 1, 5, 0, time.UTC)
	rows := [][]driver.Value{
		{uuid.NewString(), "delivered", int64(3), time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC), delivered},
		{uuid.NewString(), "printing", int64(1), time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC), nil},
	}
	srv := newAuthTestServer(t, listJobsQueries(rows))

	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	req = req.WithContext(authctx.WithSender(req.Context(), uuid.NewString(), "sender"))
	rec := httptest.NewRecorder()
	srv.ListMyJobs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Jobs []struct {
			JobID       string  `json:"job_id"`
			Status      string  `json:"status"`
			PageCount   int32   `json:"page_count"`
			CreatedAt   string  `json:"created_at"`
			DeliveredAt *string `json:"delivered_at"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(resp.Jobs))
	}
	if resp.Jobs[0].Status != "delivered" || resp.Jobs[0].DeliveredAt == nil {
		t.Errorf("first job: got status=%q delivered_at=%v", resp.Jobs[0].Status, resp.Jobs[0].DeliveredAt)
	}
	if resp.Jobs[1].DeliveredAt != nil {
		t.Errorf("second job (not delivered) should have null delivered_at, got %v", *resp.Jobs[1].DeliveredAt)
	}
	// Zero-knowledge: history must never carry ciphertext key material or the
	// blob pointer.
	if body := rec.Body.String(); strings.Contains(body, "encrypted_key") || strings.Contains(body, "blob_ref") {
		t.Errorf("history response leaked key/blob fields: %s", body)
	}
}

func TestListMyJobs_RequiresAuth(t *testing.T) {
	// No authctx sender in the request context (a guest): defensive 401.
	unreached := func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		return nil, nil, fmt.Errorf("query should not run without a sender: %.40s", query)
	}
	srv := newAuthTestServer(t, unreached)
	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	rec := httptest.NewRecorder()
	srv.ListMyJobs(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}
