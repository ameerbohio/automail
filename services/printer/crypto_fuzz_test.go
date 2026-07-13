package main

// Fuzz the document-decrypt boundary (Testing Goal T4 / Part 1). DecryptDocument
// runs on ciphertext that arrives over the network, so it must treat every byte
// as adversarial: never panic, and never yield plaintext for input it can't
// authenticate. Seeded with a valid AES-256-GCM vector so the fuzzer explores
// near-valid inputs (truncated tags, flipped IVs) as well as pure garbage.
//
//	go test -run '^$' -fuzz FuzzDecryptDocument -fuzztime=30s .

import (
	"crypto/aes"
	"crypto/cipher"
	"testing"
)

// mustSealGCM builds a valid [12-byte IV || ciphertext+tag] blob for the corpus.
// A deterministic zero IV is fine here — this is seed data, not production.
func mustSealGCM(key, pt []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	iv := make([]byte, gcm.NonceSize())
	return append(append([]byte{}, iv...), gcm.Seal(nil, iv, pt, nil)...)
}

func FuzzDecryptDocument(f *testing.F) {
	key := make([]byte, 32) // AES-256; fixed so only the ciphertext varies
	for i := range key {
		key[i] = byte(i)
	}
	f.Add(mustSealGCM(key, []byte("%PDF-1.4 fuzz seed %%EOF"))) // valid
	f.Add(mustSealGCM(key, []byte{}))                           // valid, empty plaintext
	f.Add([]byte{})                                             // shorter than the IV
	f.Add(make([]byte, 12))                                     // IV only, no ciphertext/tag
	f.Add([]byte("not a real ciphertext at all"))

	f.Fuzz(func(t *testing.T, ciphertext []byte) {
		// The contract: return (plaintext, nil) or (nil, err) — never panic, and
		// never return plaintext AND an error. Correctness of the crypto is
		// covered by the round-trip/tamper unit tests; here we only assert the
		// parser survives hostile bytes.
		out, err := DecryptDocument(ciphertext, key)
		if err != nil && out != nil {
			t.Fatalf("DecryptDocument returned bytes (%d) alongside an error: %v", len(out), err)
		}
	})
}
