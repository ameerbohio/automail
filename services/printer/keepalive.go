package main

import (
	"context"
	"log"
	"time"

	"nhooyr.io/websocket"
)

// runKeepalive pings every interval to keep the connection alive and prove
// liveness to both sides, AND re-sends a state frame each tick so the cloud's
// printer-liveness cache (mailbox:<id>:state, 90s TTL) stays fresh while the
// socket is up (plans/04-printer-microservice.md "Keepalive and State
// Reporting"). Without the periodic state frame a connected-but-idle printer
// would fall out of that cache after the TTL and stop being dispatched to,
// even though its socket is perfectly healthy -- the ping alone never touches
// the cache, only register/state frames do. A failed ping (including a missed
// pong -- Ping blocks until the pong arrives or ctx is done) returns, which
// the caller treats as a dead connection and tears down for reconnect.
//
// nhooyr.io/websocket replies to the *peer's* pings automatically; this
// loop only covers the direction this process initiates.
func runKeepalive(ctx context.Context, conn *websocket.Conn, interval time.Duration, state *slotState, send func(Frame)) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Refresh the cloud liveness cache. Uses the shared send closure so
			// writes stay serialized with dispatch status frames.
			status, slots := state.snapshot()
			send(Frame{Type: "state", Status: status, SlotOccupancy: slots})

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
