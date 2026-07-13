package handlers

import (
	"database/sql"
	"math"
	"net/http"
	"strconv"
	"time"

	"automail/cloud/db"
	"automail/cloud/store"
)

// The ops dashboard (plans/07-ops-dashboard.md, roadmap Phase 9) is a
// read-only, metadata-only window into system state for an operator. Every
// handler in this file is behind the admin-role guard (requireAdmin,
// middleware.go) and, like the rest of the handlers package, sits inside the
// zero-knowledge boundary: none of them read, return, or log encrypted_key or
// blob_ref. The sqlc queries they use don't even select those columns.

// defaultPerPage / maxPerPage bound GET /admin/jobs pagination. 50 is the
// plans/07 default; the cap keeps a hand-crafted ?per_page= from pulling an
// unbounded result set.
const (
	defaultPerPage = 50
	maxPerPage     = 200
)

type adminJob struct {
	JobID       string  `json:"job_id"`
	SlotID      string  `json:"slot_id"`
	SlotNumber  int32   `json:"slot_number"`
	Status      string  `json:"status"`
	PageCount   int32   `json:"page_count"`
	CreatedAt   string  `json:"created_at"`
	DeliveredAt *string `json:"delivered_at,omitempty"`
}

type adminJobsResponse struct {
	Jobs  []adminJob `json:"jobs"`
	Total int64      `json:"total"`
	Page  int        `json:"page"`
}

// AdminJobs handles GET /admin/jobs (plans/09-api-contracts.md): the operator
// job table, newest first, with an optional exact-status filter and 50-per-page
// pagination. Metadata only -- the AdminListJobs query never selects
// encrypted_key or blob_ref, so no ciphertext can reach this response.
func (s *Server) AdminJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Empty status = the "All" filter (AdminListJobs treats '' as match-any).
	// An unrecognized status simply matches no rows -- no need to reject it.
	status := q.Get("status")

	page := atoiDefault(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	perPage := atoiDefault(q.Get("per_page"), defaultPerPage)
	if perPage < 1 {
		perPage = defaultPerPage
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}
	offset := (page - 1) * perPage
	// Clamp so a hand-crafted ?page= can't overflow the int32 RowOffset below
	// (a negative offset would make Postgres reject the query). math.MaxInt32
	// rows is already far past any real admin page.
	if offset > math.MaxInt32 {
		offset = math.MaxInt32
	}

	rows, err := s.Queries.AdminListJobs(r.Context(), db.AdminListJobsParams{
		Status:    status,
		RowLimit:  int32(perPage),
		RowOffset: int32(offset),
	})
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not list jobs", "INTERNAL")
		return
	}
	total, err := s.Queries.AdminCountJobs(r.Context(), status)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not count jobs", "INTERNAL")
		return
	}

	jobs := make([]adminJob, 0, len(rows))
	for _, row := range rows {
		aj := adminJob{
			JobID:      row.ID.String(),
			SlotID:     row.SlotID.String(),
			SlotNumber: row.SlotNumber,
			Status:     row.Status,
			PageCount:  row.PageCount,
			CreatedAt:  row.CreatedAt.UTC().Format(time.RFC3339),
		}
		if row.DeliveredAt.Valid {
			delivered := row.DeliveredAt.Time.UTC().Format(time.RFC3339)
			aj.DeliveredAt = &delivered
		}
		jobs = append(jobs, aj)
	}
	WriteJSON(w, http.StatusOK, adminJobsResponse{Jobs: jobs, Total: total, Page: page})
}

type slotOccupancy struct {
	SlotNumber int32 `json:"slot_number"`
	Current    int   `json:"current"`
	Max        int   `json:"max"`
}

type adminMailbox struct {
	MailboxID       string                   `json:"mailbox_id"`
	BuildingAddress string                   `json:"building_address"`
	Status          string                   `json:"status"`
	LastHeartbeatAt *string                  `json:"last_heartbeat_at,omitempty"`
	SlotOccupancy   map[string]slotOccupancy `json:"slot_occupancy"`
}

type adminMailboxesResponse struct {
	Mailboxes []adminMailbox `json:"mailboxes"`
}

// AdminMailboxes handles GET /admin/mailboxes (plans/09-api-contracts.md):
// per-unit status + slot occupancy. The stored mailbox row supplies address and
// last heartbeat; live status and occupancy come from the Redis
// mailbox:<id>:state cache the printer-link hub refreshes (the DB status column
// is never updated -- the hub writes Redis, not Postgres). A mailbox with no
// live cache entry is reported "offline" (plans/07: offline = no recent
// heartbeat), with slot capacity from the DB and current occupancy 0.
func (s *Server) AdminMailboxes(w http.ResponseWriter, r *http.Request) {
	mailboxes, err := s.Queries.AdminListMailboxes(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not list mailboxes", "INTERNAL")
		return
	}
	slots, err := s.Queries.AdminListSlots(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not list slots", "INTERNAL")
		return
	}

	slotsByMailbox := make(map[string][]db.AdminListSlotsRow)
	for _, slot := range slots {
		key := slot.MailboxID.String()
		slotsByMailbox[key] = append(slotsByMailbox[key], slot)
	}

	out := make([]adminMailbox, 0, len(mailboxes))
	for _, mb := range mailboxes {
		mbID := mb.MailboxID.String()

		state, live, err := store.LookupPrinterState(r.Context(), s.Redis, mbID)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "could not read printer state", "INTERNAL")
			return
		}

		// Live cache present -> the printer's own reported status (idle |
		// printing). Absent -> offline (TTL lapsed or never registered).
		status := "offline"
		if live {
			status = state.Status
			if status == "" {
				status = "idle"
			}
		}

		occupancy := make(map[string]slotOccupancy, len(slotsByMailbox[mbID]))
		for _, slot := range slotsByMailbox[mbID] {
			// max is the DB-authoritative capacity; current is overlaid from
			// the live cache when the printer has reported this slot.
			current := 0
			if live {
				if s, ok := state.SlotOccupancy[slot.ID.String()]; ok {
					current = s.Current
				}
			}
			occupancy[slot.ID.String()] = slotOccupancy{
				SlotNumber: slot.SlotNumber,
				Current:    current,
				Max:        int(slot.MaxCount),
			}
		}

		am := adminMailbox{
			MailboxID:       mbID,
			BuildingAddress: mb.BuildingAddress,
			Status:          status,
			SlotOccupancy:   occupancy,
		}
		if mb.LastHeartbeatAt.Valid {
			hb := mb.LastHeartbeatAt.Time.UTC().Format(time.RFC3339)
			am.LastHeartbeatAt = &hb
		}
		out = append(out, am)
	}
	WriteJSON(w, http.StatusOK, adminMailboxesResponse{Mailboxes: out})
}

type adminSummaryResponse struct {
	StatusCounts   map[string]int64 `json:"status_counts"`
	QueueDepth     int64            `json:"queue_depth"`
	CompletedToday int64            `json:"completed_today"`
}

// AdminSummary handles GET /admin/summary: the aggregate figures the /admin
// overview shows (plans/07-ops-dashboard.md "Overview"). It is the one admin
// number the two list endpoints cannot cheaply produce -- queue depth would
// need one call per status, and "completed today" needs a time-bounded count
// the paginated job list can't express. Metadata only: pure counts, no job
// identifiers, no ciphertext.
func (s *Server) AdminSummary(w http.ResponseWriter, r *http.Request) {
	counts, err := s.Queries.AdminJobStatusCounts(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not count jobs", "INTERNAL")
		return
	}

	statusCounts := make(map[string]int64, len(counts))
	var queueDepth int64
	for _, c := range counts {
		statusCounts[c.Status] = c.Count
		// Queue depth = work waiting to reach a printer: submitted/queued
		// (not yet dispatched) plus dispatching (in flight to the printer).
		// printing is in-progress, not queued (plans/07 "in queue
		// (pending/dispatching)").
		switch c.Status {
		case "submitted", "queued", "dispatching":
			queueDepth += c.Count
		}
	}

	// Start of the current UTC day -- the boundary for "completed today".
	now := time.Now().UTC()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	completedToday, err := s.Queries.AdminCountDeliveredSince(r.Context(), sql.NullTime{Time: startOfDay, Valid: true})
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not count deliveries", "INTERNAL")
		return
	}

	WriteJSON(w, http.StatusOK, adminSummaryResponse{
		StatusCounts:   statusCounts,
		QueueDepth:     queueDepth,
		CompletedToday: completedToday,
	})
}

// atoiDefault parses a query-string integer, falling back to def for an empty
// or malformed value -- pagination params are advisory, so a garbled ?page=
// just uses the default rather than erroring the whole request.
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
