package main

// Nasty-row edge-case tables for the printer's pure logic (Testing Goal T4 /
// Part 1). These complement the happy-path unit tests with the boundary inputs
// most likely to hide a bug: empty/misaligned buffers, out-of-range pad bytes,
// and integer-overflowing backoff attempts.

import (
	"bytes"
	"testing"
	"time"
)

func TestPKCS7Unpad_NastyRows(t *testing.T) {
	const bs = 16 // AES block size, the real caller's value
	tests := []struct {
		name    string
		in      []byte
		want    []byte
		wantErr bool
	}{
		{"empty input", nil, nil, true},
		{"not block aligned (15)", bytes.Repeat([]byte{0x01}, 15), nil, true},
		{"full block of pad", bytes.Repeat([]byte{byte(bs)}, bs), []byte{}, false},
		{"valid 3-byte pad", append(bytes.Repeat([]byte{'x'}, 13), 0x03, 0x03, 0x03), bytes.Repeat([]byte{'x'}, 13), false},
		{"pad byte zero", append(bytes.Repeat([]byte{'x'}, 15), 0x00), nil, true},
		{"pad byte exceeds block size", append(bytes.Repeat([]byte{'x'}, 15), byte(bs+1)), nil, true},
		{"inconsistent pad bytes", append(bytes.Repeat([]byte{'x'}, 14), 0x02, 0x03), nil, true},
		{"full-block pad claim but content differs", append(bytes.Repeat([]byte{'x'}, 15), byte(bs)), nil, true}, // last byte 0x10 claims a 16-byte pad, but the other 15 bytes aren't 0x10
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pkcs7Unpad(tc.in, bs)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBackoffWithJitter_NastyRows(t *testing.T) {
	const maxBackoff = 100 * time.Second
	// The invariant for maxBackoff > 0: the result is always in [0, maxBackoff],
	// for every attempt — including negatives and shift-overflowing values.
	for _, attempt := range []int{-100, -1, 0, 1, 30, 62, 63, 64, 65, 1000} {
		for i := 0; i < 50; i++ { // sample the jitter
			got := backoffWithJitter(attempt, maxBackoff)
			if got < 0 || got > maxBackoff {
				t.Fatalf("attempt %d: backoffWithJitter = %s, want in [0, %s]", attempt, got, maxBackoff)
			}
		}
	}
	// Non-positive maxBackoff must yield exactly zero (no rand.Int63n(0) panic).
	for _, mb := range []time.Duration{0, -1 * time.Second} {
		if got := backoffWithJitter(5, mb); got != 0 {
			t.Fatalf("maxBackoff %s: got %s, want 0", mb, got)
		}
	}
}
