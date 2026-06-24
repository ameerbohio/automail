package main

import (
	"context"
	"log"
	"time"

	"nhooyr.io/websocket"
)

// runKeepalive pings every interval to keep the connection alive and
// prove liveness to both sides. A failed ping (including a missed pong --
// Ping blocks until the pong arrives or ctx is done) returns, which the
// caller treats as a dead connection and tears down for reconnect
// (plans/04-printer-microservice.md "Keepalive and State Reporting").
//
// nhooyr.io/websocket replies to the *peer's* pings automatically; this
// loop only covers the direction this process initiates.
func runKeepalive(ctx context.Context, conn *websocket.Conn, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, interval)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				log.Printf("printer: ping failed: %v", err)
				return err
			}
		}
	}
}
