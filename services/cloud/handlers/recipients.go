package handlers

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"automail/cloud/db"

	"github.com/google/uuid"
)

type recipientSummary struct {
	RecipientID     string `json:"recipient_id"`
	DisplayName     string `json:"display_name"`
	BuildingAddress string `json:"building_address"`
}

// maskName reduces a full name to "first initial + last name" so the
// sender never sees a resident's full name from a search result --
// plans/09-api-contracts.md: "Full name is never returned."
func maskName(full string) string {
	parts := strings.Fields(full)
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0]
	}
	first := parts[0]
	last := parts[len(parts)-1]
	initial := first[:1]
	return initial + ". " + last
}

// SearchRecipients handles GET /recipients?q=<name or address>. No auth --
// rate-limited at Traefik instead (plans/09-api-contracts.md).
func (s *Server) SearchRecipients(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if len(q) < 2 {
		writeError(w, http.StatusBadRequest, "q must be at least 2 characters", "INVALID_QUERY")
		return
	}

	rows, err := s.Queries.SearchRecipients(r.Context(), db.SearchRecipientsParams{
		AppKey: s.AppKey,
		Query:  q,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search failed", "INTERNAL")
		return
	}

	results := make([]recipientSummary, 0, len(rows))
	for _, row := range rows {
		results = append(results, recipientSummary{
			RecipientID:     row.RecipientID.String(),
			DisplayName:     maskName(row.FullName),
			BuildingAddress: row.BuildingAddress,
		})
	}
	writeJSON(w, http.StatusOK, results)
}

type publicKeyResponse struct {
	RecipientID  string `json:"recipient_id"`
	PublicKeyPem string `json:"public_key_pem"`
}

// RecipientPublicKey handles GET /recipients/{id}/public-key. No auth.
// The cloud server resolves recipient -> mailbox internally; the sender
// never observes the mailbox or slot ID (plans/09-api-contracts.md).
func (s *Server) RecipientPublicKey(w http.ResponseWriter, r *http.Request) {
	idParam := r.PathValue("id")
	recipientID, err := uuid.Parse(idParam)
	if err != nil {
		writeError(w, http.StatusNotFound, "recipient not found", "RECIPIENT_NOT_FOUND")
		return
	}

	resolved, err := s.Queries.ResolveRecipient(r.Context(), recipientID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "recipient not found or slot unassigned", "RECIPIENT_NOT_FOUND")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed", "INTERNAL")
		return
	}

	writeJSON(w, http.StatusOK, publicKeyResponse{
		RecipientID:  resolved.RecipientID.String(),
		PublicKeyPem: resolved.PublicKeyPem,
	})
}
