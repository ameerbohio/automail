package main

import (
	"log"
	"os"
	"strconv"
	"time"
)

// config holds every environment variable the printer microservice reads
// at startup (plans/04-printer-microservice.md "Configuration"). No
// config file, no flags -- env vars are enough for a single-process
// service with this few knobs.
type config struct {
	MailboxID string
	// SlotID is the identifier this unit's single slot reports occupancy
	// under. plans/04-printer-microservice.md: slot_occupancy is keyed by
	// "<slot_id>", and the cloud's dispatch eligibility check looks the slot
	// up by the DB mailbox_slots.id -- so a real deployment sets this to that
	// UUID (like MAILBOX_ID). Defaults to "slot-1" for standalone/dev.
	SlotID            string
	CloudServerWSURL  string
	MTLSCACertPath    string
	MTLSCertPath      string
	MTLSKeyPath       string
	HeartbeatInterval time.Duration
	ReconnectMaxBack  time.Duration
	DevMode           bool
	ListenAddr        string
	PrinterName       string
	// PrinterPrivateKeyPath points at the encrypted RSA document key. It is
	// required in every mode, including DEV_MODE: dev mode still runs the
	// full decrypt pipeline and only skips the physical `lp` call. The
	// passphrase (PRINTER_KEY_PASSPHRASE) is read directly in main.go, not
	// stored here, so it lives no longer than the one-time key load.
	PrinterPrivateKeyPath string
}

func mustEnv(key string) string {
	val := os.Getenv(key)
	if val == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return val
}

func envDurationSeconds(key string, def time.Duration) time.Duration {
	val := os.Getenv(key)
	if val == "" {
		return def
	}
	secs, err := strconv.Atoi(val)
	if err != nil {
		log.Fatalf("env var %s must be an integer number of seconds, got %q", key, val)
	}
	return time.Duration(secs) * time.Second
}

func loadConfig() config {
	listenAddr := os.Getenv("LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = ":8444"
	}
	slotID := os.Getenv("SLOT_ID")
	if slotID == "" {
		slotID = "slot-1"
	}
	return config{
		MailboxID:         mustEnv("MAILBOX_ID"),
		SlotID:            slotID,
		CloudServerWSURL:  mustEnv("CLOUD_SERVER_WS_URL"),
		MTLSCACertPath:    mustEnv("MTLS_CA_CERT_PATH"),
		MTLSCertPath:      mustEnv("MTLS_CERT_PATH"),
		MTLSKeyPath:       mustEnv("MTLS_KEY_PATH"),
		HeartbeatInterval: envDurationSeconds("HEARTBEAT_INTERVAL", 30*time.Second),
		ReconnectMaxBack:  envDurationSeconds("RECONNECT_MAX_BACKOFF", 30*time.Second),
		DevMode:           os.Getenv("DEV_MODE") == "true",
		ListenAddr:        listenAddr,
		PrinterName:       os.Getenv("PRINTER_NAME"), // used by the lp call in non-dev mode

		PrinterPrivateKeyPath: mustEnv("PRINTER_PRIVATE_KEY_PATH"),
	}
}
