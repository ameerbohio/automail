// Command printer is the Automail printer microservice: the only
// component in the system that ever touches plaintext
// (plans/04-printer-microservice.md). It dials out to the cloud server,
// holds a persistent mTLS WebSocket open, and processes dispatch frames
// pushed down that socket.
//
// It dials out to the cloud server, decrypts each dispatched job in RAM
// (RSA-OAEP unwrap + AES-256-GCM), prints via CUPS, and wipes the
// plaintext. DEV_MODE keeps the full decrypt pipeline but skips the
// physical `lp` call.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	cfg := loadConfig()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	key, err := loadDocKey(cfg.PrinterPrivateKeyPath)
	if err != nil {
		log.Fatalf("printer: load document private key: %v", err)
	}

	state := newSlotState()

	go startHealthServer(cfg.ListenAddr)

	log.Printf("printer: starting (mailbox_id=%s dev_mode=%v)", cfg.MailboxID, cfg.DevMode)
	runClient(ctx, cfg, state, key)
	log.Printf("printer: shutting down")
}

// loadDocKey reads the passphrase from PRINTER_KEY_PASSPHRASE, decrypts the
// document private key, and disposes of the passphrase as fast as the
// runtime allows: it unsets the env var (so child processes and
// /proc/self/environ can't read it) and zeroes the mutable []byte copy
// after the key is derived. The passphrase's origin -- os.Getenv's return
// string -- is immutable and cannot be wiped; it lingers until GC. That
// residual window is the honest limit documented in
// docs/study/16-hybrid-encryption.md.
func loadDocKey(keyPath string) (*printerKey, error) {
	pass := os.Getenv("PRINTER_KEY_PASSPHRASE")
	if pass == "" {
		return nil, errors.New("PRINTER_KEY_PASSPHRASE is not set")
	}
	os.Unsetenv("PRINTER_KEY_PASSPHRASE")

	passBytes := []byte(pass)
	defer zeroBytes(passBytes)

	pemBytes, err := os.ReadFile(keyPath) // #nosec G304 -- operator-configured printer key path, not user input
	if err != nil {
		return nil, err
	}
	return loadPrinterPrivateKey(pemBytes, passBytes)
}
