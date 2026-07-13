package main

import (
	"context"
	"log"
	"math/rand"
	"net/http"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// runClient owns the dial-reconnect loop for the life of the process: dial,
// register, run the connection until it errors, then back off and dial
// again. It never returns except via ctx cancellation
// (plans/04-printer-microservice.md "Connection lifecycle").
func runClient(ctx context.Context, cfg config, state *slotState, key *printerKey) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if err := connectAndServe(ctx, cfg, state, key); err != nil {
			log.Printf("printer: connection attempt failed: %v", err)
		}
		setConnected(false)

		attempt++
		wait := backoffWithJitter(attempt, cfg.ReconnectMaxBack)
		log.Printf("printer: reconnecting in %s (attempt %d)", wait, attempt)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// backoffWithJitter is exponential backoff (1s, 2s, 4s, 8s, ...) capped at
// maxBackoff, with full jitter (a random value in [0, cap)) so that many
// printers reconnecting after a shared outage don't all hammer the cloud
// server in the same instant (the "thundering herd" problem).
func backoffWithJitter(attempt int, maxBackoff time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := time.Duration(1<<uint(attempt-1)) * time.Second
	if base > maxBackoff || base <= 0 {
		base = maxBackoff
	}
	if base <= 0 {
		return 0
	}
	// #nosec G404 -- math/rand is fine for reconnect-backoff jitter; it is not security-sensitive.
	return time.Duration(rand.Int63n(int64(base)))
}

// connectAndServe dials once, registers, and runs the connection's
// read/keepalive loops until one of them errors -- at which point the
// connection is considered dead and the caller reconnects.
func connectAndServe(ctx context.Context, cfg config, state *slotState, key *printerKey) error {
	tlsConfig, err := newMTLSClientConfig(cfg)
	if err != nil {
		return err
	}
	httpClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
	}

	conn, _, err := websocket.Dial(ctx, cfg.CloudServerWSURL, &websocket.DialOptions{
		HTTPClient: httpClient,
	})
	if err != nil {
		return err
	}
	defer conn.CloseNow()

	status, slots := state.snapshot()
	if err := wsjson.Write(ctx, conn, Frame{
		Type:          "register",
		MailboxID:     cfg.MailboxID,
		SlotOccupancy: slots,
	}); err != nil {
		return err
	}
	log.Printf("printer: registered mailbox %s with cloud server", cfg.MailboxID)
	setConnected(true)

	// Send an initial state frame right after register so the cloud's
	// cache has a real status word, not just the "idle, no status yet"
	// seed it wrote from the register frame alone.
	if err := wsjson.Write(ctx, conn, Frame{Type: "state", Status: status, SlotOccupancy: slots}); err != nil {
		return err
	}

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	send := func(frame Frame) {
		if err := wsjson.Write(connCtx, conn, frame); err != nil {
			log.Printf("printer: failed to send %s frame: %v", frame.Type, err)
		}
	}

	errCh := make(chan error, 2)
	go func() { errCh <- runKeepalive(connCtx, conn, cfg.HeartbeatInterval) }()
	go func() { errCh <- readLoop(connCtx, conn, cfg, state, key, send) }()

	err = <-errCh
	cancel()
	return err
}

// readLoop receives dispatch frames and hands each to its own goroutine
// (plans/04-printer-microservice.md "handed to a worker goroutine so the
// read loop stays free for keepalive and further frames").
func readLoop(ctx context.Context, conn *websocket.Conn, cfg config, state *slotState, key *printerKey, send func(Frame)) error {
	for {
		var frame Frame
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			return err
		}
		switch frame.Type {
		case "dispatch":
			go handleDispatch(ctx, frame, cfg, state, key, send)
		default:
			log.Printf("printer: ignoring unknown frame type %q", frame.Type)
		}
	}
}
