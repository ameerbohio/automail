package main

import (
	"context"
	"log"
	"time"
)

// defaultShutdownGrace bounds the whole shutdown sequence. Kubernetes sends
// SIGTERM, waits terminationGracePeriodSeconds, then SIGKILLs, so this must
// stay strictly below that grace minus any preStop sleep -- the arithmetic is
// written into the Deployment manifest (plans/16-kubernetes.md §5). Under
// Compose the equivalent bound is `docker compose stop -t` (default 10s), so
// the default here is a value both substrates tolerate.
const defaultShutdownGrace = 20 * time.Second

// shutdownStep is one named phase of the graceful shutdown, run in order.
type shutdownStep struct {
	name string
	run  func(context.Context) error
}

// runShutdown executes the steps in order under a single shared deadline,
// logging each. A step that fails does NOT abort the sequence: the later
// steps (draining HTTP, closing the internal listener) are exactly the ones
// that matter most when something has already gone wrong, and every step here
// is independently safe to attempt. The shared deadline is what keeps a
// wedged step from consuming the whole grace period on its own -- each step's
// context is already past its deadline, so it returns immediately.
func runShutdown(ctx context.Context, steps []shutdownStep) {
	for _, step := range steps {
		start := time.Now()
		if err := step.run(ctx); err != nil {
			log.Printf("shutdown: %s failed after %s: %v", step.name, time.Since(start).Round(time.Millisecond), err)
			continue
		}
		log.Printf("shutdown: %s done in %s", step.name, time.Since(start).Round(time.Millisecond))
	}
}

// waitFor blocks until done is closed or ctx expires, reporting ctx's error
// if the wait timed out. Used to join a goroutine (the dispatcher loop)
// inside the shutdown deadline instead of unconditionally.
func waitFor(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
