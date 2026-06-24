// Command printer is the Automail printer microservice: the only
// component in the system that ever touches plaintext
// (plans/04-printer-microservice.md). It dials out to the cloud server,
// holds a persistent mTLS WebSocket open, and processes dispatch frames
// pushed down that socket.
//
// Phase 3 implements the dev-mode stub: connection lifecycle, framing,
// and a fake "print" that never decrypts anything. Real RSA-OAEP/AES-GCM
// decryption and CUPS printing land in Phase 6.
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"
)

func main() {
	cfg := loadConfig()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	state := newSlotState()

	go startHealthServer(cfg.ListenAddr)

	log.Printf("printer: starting (mailbox_id=%s dev_mode=%v)", cfg.MailboxID, cfg.DevMode)
	runClient(ctx, cfg, state)
	log.Printf("printer: shutting down")
}
