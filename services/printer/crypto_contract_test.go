//go:build contract

// Cross-language crypto contract, printer side (Testing Goal T2 / Part 3).
//
// Tagged `contract` so it is excluded from the normal `go test ./...`, race, and
// coverage runs: these tests exchange generated vector files that only exist
// during `make crypto-contract`, which drives the two languages in order.
//
//	Direction A (production path): the browser encrypts, the printer decrypts.
//	  TestContractPrinterDecryptsBrowser reads the browser-produced vector and
//	  asserts DecryptAESKey + DecryptDocument reproduce the exact input bytes,
//	  then that a one-bit flip in either the ciphertext or the wrapped key is
//	  rejected (GCM / OAEP), never silently partially decrypted.
//	Direction B (guard): Go encrypts exactly as the browser would; the browser
//	  test (crypto-contract.contract.test.ts) decrypts it.
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

const contractDir = "../../testdata/crypto-contract"

type contractFixture struct {
	PublicSPKIPEM   string `json:"public_spki_pem"`
	PrivatePKCS8PEM string `json:"private_pkcs8_pem"`
}

type contractVector struct {
	InputB64        string `json:"input_b64"`
	EncryptedKeyB64 string `json:"encrypted_key_b64"`
	CiphertextB64   string `json:"ciphertext_b64"`
}

func loadFixture(t *testing.T) contractFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractDir, "fixture.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f contractFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return f
}

func decodeB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	return b
}

// TestContractPrinterDecryptsBrowser is Direction A: the printer must reproduce
// the browser's plaintext byte-for-byte, and must reject tampering.
func TestContractPrinterDecryptsBrowser(t *testing.T) {
	f := loadFixture(t)
	// Exercise the real key loader (unencrypted PKCS#8 PRIVATE KEY path).
	key, err := loadPrinterPrivateKey([]byte(f.PrivatePKCS8PEM), nil)
	if err != nil {
		t.Fatalf("load printer key: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(contractDir, "browser_to_printer.vector.json"))
	if err != nil {
		t.Fatalf("read browser vector (run `make crypto-contract`, not `go test`): %v", err)
	}
	var v contractVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse vector: %v", err)
	}
	want := decodeB64(t, v.InputB64)
	encKey := decodeB64(t, v.EncryptedKeyB64)
	ciphertext := decodeB64(t, v.CiphertextB64)

	aesKey, err := key.DecryptAESKey(encKey)
	if err != nil {
		t.Fatalf("unwrap AES key (OAEP mismatch?): %v", err)
	}
	got, err := DecryptDocument(ciphertext, aesKey)
	if err != nil {
		t.Fatalf("decrypt document (GCM/IV mismatch?): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("plaintext mismatch: got %d bytes, want %d", len(got), len(want))
	}
	t.Logf("Direction A: printer reproduced %d browser-encrypted bytes exactly", len(got))

	// Tamper 1: flip the last ciphertext byte -> GCM auth must fail.
	badCT := append([]byte(nil), ciphertext...)
	badCT[len(badCT)-1] ^= 0x01
	if out, err := DecryptDocument(badCT, aesKey); err == nil {
		t.Fatalf("tampered ciphertext decrypted without error (%d bytes) — GCM auth not enforced", len(out))
	}
	// Tamper 2: flip the last wrapped-key byte -> OAEP unwrap must fail.
	badKey := append([]byte(nil), encKey...)
	badKey[len(badKey)-1] ^= 0x01
	if _, err := key.DecryptAESKey(badKey); err == nil {
		t.Fatal("tampered encrypted_key unwrapped without error — OAEP not enforced")
	}
	t.Log("Direction A: one-bit tampering rejected by both GCM and OAEP")
}

// TestContractGoEncryptForBrowser is Direction B step 1: produce a vector, in the
// exact wire format encrypt.ts uses, for the browser test to decrypt.
func TestContractGoEncryptForBrowser(t *testing.T) {
	f := loadFixture(t)
	block, _ := pem.Decode([]byte(f.PublicSPKIPEM))
	if block == nil {
		t.Fatal("no PEM block in fixture public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse SPKI public key: %v", err)
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok {
		t.Fatalf("fixture public key is %T, want *rsa.PublicKey", parsed)
	}

	input := make([]byte, 512)
	if _, err := rand.Read(input); err != nil {
		t.Fatal(err)
	}
	aesKey := make([]byte, 32) // AES-256
	if _, err := rand.Read(aesKey); err != nil {
		t.Fatal(err)
	}
	encKey, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, aesKey, nil)
	if err != nil {
		t.Fatalf("RSA-OAEP wrap: %v", err)
	}
	c, err := aes.NewCipher(aesKey)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(c)
	if err != nil {
		t.Fatal(err)
	}
	iv := make([]byte, gcm.NonceSize()) // 12 bytes, matches the browser
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	blob := append(append([]byte(nil), iv...), gcm.Seal(nil, iv, input, nil)...)

	out, err := json.MarshalIndent(contractVector{
		InputB64:        base64.StdEncoding.EncodeToString(input),
		EncryptedKeyB64: base64.StdEncoding.EncodeToString(encKey),
		CiphertextB64:   base64.StdEncoding.EncodeToString(blob),
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contractDir, "go_to_browser.vector.json"), out, 0o644); err != nil {
		t.Fatalf("write go->browser vector: %v", err)
	}
	t.Logf("Direction B: wrote a Go-encrypted vector (%d-byte input) for the browser", len(input))
}
