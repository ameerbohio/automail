package handlers

import (
	"log"
	"net/http"
)

// PrinterLink handles GET /internal/printer-link, the mTLS WebSocket the
// printer microservice dials out to and holds open
// (plans/09-api-contracts.md "Internal Link"). Reaching this handler at
// all already proves the caller presented a client cert signed by the
// internal CA -- the mTLS listener's tls.RequireAndVerifyClientCert
// rejects the handshake before any handler runs otherwise, exactly like
// /internal/healthz.
func (s *Server) PrinterLink(w http.ResponseWriter, r *http.Request) {
	if err := s.Hub.Accept(r.Context(), w, r); err != nil {
		log.Printf("printer-link: connection ended: %v", err)
	}
}
