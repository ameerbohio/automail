package main

import (
	"testing"
	"time"
)

func TestBackoffWithJitter_Doubles(t *testing.T) {
	maxBackoff := 100 * time.Second
	// Full jitter returns a value in [0, base) -- check the *cap* doubles
	// per attempt rather than the (randomized) returned value itself.
	caps := []time.Duration{1, 2, 4, 8, 16}
	for i, want := range caps {
		base := time.Duration(1<<uint(i)) * time.Second
		if base != want*time.Second {
			t.Fatalf("attempt %d: expected base %s, got %s", i+1, want*time.Second, base)
		}
		got := backoffWithJitter(i+1, maxBackoff)
		if got < 0 || got >= base {
			t.Errorf("attempt %d: backoffWithJitter returned %s, want in [0, %s)", i+1, got, base)
		}
	}
}

func TestBackoffWithJitter_CapsAtMax(t *testing.T) {
	maxBackoff := 5 * time.Second
	// Attempt 10 would uncapped be 2^9 = 512s; must be clamped to maxBackoff.
	for i := 0; i < 20; i++ {
		got := backoffWithJitter(10, maxBackoff)
		if got < 0 || got >= maxBackoff {
			t.Fatalf("backoffWithJitter(10, %s) = %s, want in [0, %s)", maxBackoff, got, maxBackoff)
		}
	}
}

func TestBackoffWithJitter_NeverNegativeForAttemptZero(t *testing.T) {
	got := backoffWithJitter(0, 30*time.Second)
	if got < 0 {
		t.Fatalf("backoffWithJitter(0, ...) = %s, want >= 0", got)
	}
}
