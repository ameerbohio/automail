package main

// Phase 9 tests for the ops dashboard (GET /admin/summary, /admin/jobs,
// /admin/mailboxes) and the admin-role guard. Same harness as the Phase 5/8
// tests: the real sqlc query layer over the fake database/sql driver
// (dbfake_test.go), miniredis for the printer-state cache, and httptest for the
// role middleware -- no Postgres, no real Redis server.

import (
	"context"
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
	"automail/cloud/jwtutil"
	"automail/cloud/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// newAdminTestServer builds a handlers.Server over the fake DB, optionally
// wired to a miniredis instance (nil for the endpoints that don't touch Redis).
func newAdminTestServer(t *testing.T, q fakeQueryFunc, rdb *redis.Client) *handlers.Server {
	t.Helper()
	sqlDB := sql.OpenDB(fakeConnector{q: q})
	t.Cleanup(func() { _ = sqlDB.Close() })
	return &handlers.Server{Queries: db.New(sqlDB), SQLDB: sqlDB, Redis: rdb}
}

func TestAdminJobs_MetadataOnlyAndPagination(t *testing.T) {
	delivered := time.Date(2026, 7, 9, 10, 1, 5, 0, time.UTC)
	jobRows := [][]driver.Value{
		{uuid.NewString(), uuid.NewString(), int64(3), "delivered", int64(2), time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC), delivered},
		{uuid.NewString(), uuid.NewString(), int64(1), "printing", int64(4), time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC), nil},
	}
	var seenStatusArg string
	q := func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		switch {
		case strings.HasPrefix(query, "-- name: AdminListJobs"):
			// arg order: status, row_offset, row_limit.
			if s, ok := args[0].Value.(string); ok {
				seenStatusArg = s
			}
			cols := []string{"id", "slot_id", "slot_number", "status", "page_count", "created_at", "delivered_at"}
			return cols, jobRows, nil
		case strings.HasPrefix(query, "-- name: AdminCountJobs"):
			return []string{"count"}, [][]driver.Value{{int64(142)}}, nil
		default:
			return nil, nil, fmt.Errorf("unexpected query: %.60s", query)
		}
	}
	srv := newAdminTestServer(t, q, nil)

	req := httptest.NewRequest(http.MethodGet, "/admin/jobs?status=delivered&page=1&per_page=50", nil)
	rec := httptest.NewRecorder()
	srv.AdminJobs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if seenStatusArg != "delivered" {
		t.Errorf("status filter arg = %q, want delivered", seenStatusArg)
	}
	var resp struct {
		Jobs []struct {
			JobID       string  `json:"job_id"`
			SlotID      string  `json:"slot_id"`
			SlotNumber  int32   `json:"slot_number"`
			Status      string  `json:"status"`
			PageCount   int32   `json:"page_count"`
			CreatedAt   string  `json:"created_at"`
			DeliveredAt *string `json:"delivered_at"`
		} `json:"jobs"`
		Total int64 `json:"total"`
		Page  int   `json:"page"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 142 || resp.Page != 1 {
		t.Errorf("pagination: total=%d page=%d, want 142/1", resp.Total, resp.Page)
	}
	if len(resp.Jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(resp.Jobs))
	}
	if resp.Jobs[0].Status != "delivered" || resp.Jobs[0].DeliveredAt == nil {
		t.Errorf("first job: status=%q delivered_at=%v", resp.Jobs[0].Status, resp.Jobs[0].DeliveredAt)
	}
	if resp.Jobs[0].SlotNumber != 3 {
		t.Errorf("first job slot_number = %d, want 3", resp.Jobs[0].SlotNumber)
	}
	if resp.Jobs[1].DeliveredAt != nil {
		t.Errorf("second job (printing) should have null delivered_at, got %v", *resp.Jobs[1].DeliveredAt)
	}
	// Zero-knowledge: the admin job list must never carry ciphertext key
	// material or the blob pointer.
	if body := rec.Body.String(); strings.Contains(body, "encrypted_key") || strings.Contains(body, "blob_ref") {
		t.Errorf("admin job list leaked key/blob fields: %s", body)
	}
}

func TestAdminJobs_PaginationDefaultsAndClamp(t *testing.T) {
	var seenLimit, seenOffset int64
	q := func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		switch {
		case strings.HasPrefix(query, "-- name: AdminListJobs"):
			// arg order: status, row_offset, row_limit.
			seenOffset, _ = args[1].Value.(int64)
			seenLimit, _ = args[2].Value.(int64)
			return []string{"id", "slot_id", "slot_number", "status", "page_count", "created_at", "delivered_at"}, nil, nil
		case strings.HasPrefix(query, "-- name: AdminCountJobs"):
			return []string{"count"}, [][]driver.Value{{int64(0)}}, nil
		default:
			return nil, nil, fmt.Errorf("unexpected query: %.60s", query)
		}
	}
	srv := newAdminTestServer(t, q, nil)

	// per_page over the cap is clamped to 200; page 3 with the clamped size
	// gives offset 400. Garbage page falls back to 1 (tested separately below).
	req := httptest.NewRequest(http.MethodGet, "/admin/jobs?page=3&per_page=9999", nil)
	rec := httptest.NewRecorder()
	srv.AdminJobs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// maxPerPage is 200 in handlers/admin.go; per_page=9999 clamps to it.
	const wantLimit = 200
	if seenLimit != wantLimit {
		t.Errorf("row_limit = %d, want clamped %d", seenLimit, wantLimit)
	}
	if wantOffset := int64((3 - 1) * wantLimit); seenOffset != wantOffset {
		t.Errorf("row_offset = %d, want %d", seenOffset, wantOffset)
	}
}

func TestAdminSummary_QueueDepthAndCompletedToday(t *testing.T) {
	var seenSince time.Time
	q := func(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
		switch {
		case strings.HasPrefix(query, "-- name: AdminJobStatusCounts"):
			rows := [][]driver.Value{
				{"submitted", int64(2)},
				{"queued", int64(3)},
				{"dispatching", int64(1)},
				{"printing", int64(4)},
				{"delivered", int64(10)},
				{"failed", int64(1)},
			}
			return []string{"status", "count"}, rows, nil
		case strings.HasPrefix(query, "-- name: AdminCountDeliveredSince"):
			if ts, ok := args[0].Value.(time.Time); ok {
				seenSince = ts
			}
			return []string{"count"}, [][]driver.Value{{int64(7)}}, nil
		default:
			return nil, nil, fmt.Errorf("unexpected query: %.60s", query)
		}
	}
	srv := newAdminTestServer(t, q, nil)

	req := httptest.NewRequest(http.MethodGet, "/admin/summary", nil)
	rec := httptest.NewRecorder()
	srv.AdminSummary(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		StatusCounts   map[string]int64 `json:"status_counts"`
		QueueDepth     int64            `json:"queue_depth"`
		CompletedToday int64            `json:"completed_today"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// queue_depth = submitted(2)+queued(3)+dispatching(1); printing is NOT queued.
	if resp.QueueDepth != 6 {
		t.Errorf("queue_depth = %d, want 6", resp.QueueDepth)
	}
	if resp.CompletedToday != 7 {
		t.Errorf("completed_today = %d, want 7", resp.CompletedToday)
	}
	if resp.StatusCounts["delivered"] != 10 {
		t.Errorf("status_counts[delivered] = %d, want 10", resp.StatusCounts["delivered"])
	}
	// The "completed today" boundary must be midnight UTC.
	if seenSince.Hour() != 0 || seenSince.Minute() != 0 || seenSince.Location() != time.UTC {
		t.Errorf("delivered-since boundary = %v, want start-of-day UTC", seenSince)
	}
}

func TestAdminMailboxes_LiveAndOfflineStatus(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	liveMailbox := uuid.New()
	offlineMailbox := uuid.New()
	liveSlot := uuid.New()
	offlineSlot := uuid.New()

	// Seed a live printer state for one mailbox; the other has none (offline).
	if err := store.SetPrinterState(context.Background(), rdb, liveMailbox.String(), store.PrinterState{
		Status:        "printing",
		SlotOccupancy: map[string]store.SlotState{liveSlot.String(): {Current: 2, Max: 5}},
	}); err != nil {
		t.Fatalf("seed printer state: %v", err)
	}

	q := func(query string, _ []driver.NamedValue) ([]string, [][]driver.Value, error) {
		switch {
		case strings.HasPrefix(query, "-- name: AdminListMailboxes"):
			cols := []string{"mailbox_id", "building_address", "status", "last_heartbeat_at"}
			rows := [][]driver.Value{
				{liveMailbox.String(), "1 Live St", "offline", nil},
				{offlineMailbox.String(), "2 Dark Ave", "offline", nil},
			}
			return cols, rows, nil
		case strings.HasPrefix(query, "-- name: AdminListSlots"):
			cols := []string{"id", "mailbox_id", "slot_number", "max_count"}
			rows := [][]driver.Value{
				{liveSlot.String(), liveMailbox.String(), int64(1), int64(5)},
				{offlineSlot.String(), offlineMailbox.String(), int64(1), int64(5)},
			}
			return cols, rows, nil
		default:
			return nil, nil, fmt.Errorf("unexpected query: %.60s", query)
		}
	}
	srv := newAdminTestServer(t, q, rdb)

	req := httptest.NewRequest(http.MethodGet, "/admin/mailboxes", nil)
	rec := httptest.NewRecorder()
	srv.AdminMailboxes(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Mailboxes []struct {
			MailboxID     string `json:"mailbox_id"`
			Status        string `json:"status"`
			SlotOccupancy map[string]struct {
				SlotNumber int32 `json:"slot_number"`
				Current    int   `json:"current"`
				Max        int   `json:"max"`
			} `json:"slot_occupancy"`
		} `json:"mailboxes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Mailboxes) != 2 {
		t.Fatalf("got %d mailboxes, want 2", len(resp.Mailboxes))
	}
	byID := map[string]int{}
	for i, mb := range resp.Mailboxes {
		byID[mb.MailboxID] = i
	}
	live := resp.Mailboxes[byID[liveMailbox.String()]]
	if live.Status != "printing" {
		t.Errorf("live mailbox status = %q, want printing", live.Status)
	}
	if occ := live.SlotOccupancy[liveSlot.String()]; occ.Current != 2 || occ.Max != 5 {
		t.Errorf("live slot occupancy = %d/%d, want 2/5", occ.Current, occ.Max)
	}
	offline := resp.Mailboxes[byID[offlineMailbox.String()]]
	if offline.Status != "offline" {
		t.Errorf("offline mailbox status = %q, want offline", offline.Status)
	}
	// Offline printer: capacity still known from the DB, current occupancy 0.
	if occ := offline.SlotOccupancy[offlineSlot.String()]; occ.Current != 0 || occ.Max != 5 {
		t.Errorf("offline slot occupancy = %d/%d, want 0/5", occ.Current, occ.Max)
	}
}

// TestRequireAdmin_RoleGuard drives the middleware itself: no token -> 401,
// a valid sender token -> 403 (authenticated but not entitled), an admin token
// -> the wrapped handler runs.
func TestRequireAdmin_RoleGuard(t *testing.T) {
	key := newJWTKey(t)
	senderJWT, err := jwtutil.IssueAccessToken(key, uuid.New(), "sender")
	if err != nil {
		t.Fatalf("issue sender token: %v", err)
	}
	adminJWT, err := jwtutil.IssueAccessToken(key, uuid.New(), "admin")
	if err != nil {
		t.Fatalf("issue admin token: %v", err)
	}

	var reached bool
	guarded := requireAdmin(&key.PublicKey)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if role := authctx.Role(r.Context()); role != "admin" {
			t.Errorf("handler saw role %q, want admin", role)
		}
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name       string
		bearer     string
		wantStatus int
		wantReach  bool
	}{
		{"no token", "", http.StatusUnauthorized, false},
		{"sender token forbidden", senderJWT, http.StatusForbidden, false},
		{"admin token allowed", adminJWT, http.StatusOK, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(http.MethodGet, "/admin/jobs", nil)
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			rec := httptest.NewRecorder()
			guarded.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if reached != tc.wantReach {
				t.Errorf("handler reached = %v, want %v", reached, tc.wantReach)
			}
		})
	}
}
