package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestDecryptDocument_RoundTrip encrypts a payload exactly as the sender's
// Web Crypto code does -- AES-256-GCM with a 12-byte IV prepended to the
// ciphertext -- and confirms DecryptDocument recovers it.
func TestDecryptDocument_RoundTrip(t *testing.T) {
	aesKey := make([]byte, 32)
	if _, err := rand.Read(aesKey); err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("%PDF-1.4\nthe secret letter\n%%EOF")

	blob := sealGCM(t, aesKey, plaintext)

	got, err := DecryptDocument(blob, aesKey)
	if err != nil {
		t.Fatalf("DecryptDocument: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("got %q, want %q", got, plaintext)
	}
}

// TestDecryptDocument_TamperDetected proves GCM authentication: a single
// flipped ciphertext byte must fail rather than yield wrong plaintext.
func TestDecryptDocument_TamperDetected(t *testing.T) {
	aesKey := make([]byte, 32)
	rand.Read(aesKey)
	blob := sealGCM(t, aesKey, []byte("tamper me"))
	blob[len(blob)-1] ^= 0x01 // flip a bit in the tag

	if _, err := DecryptDocument(blob, aesKey); err == nil {
		t.Fatal("expected GCM auth failure on tampered ciphertext, got nil")
	}
}

func TestDecryptDocument_ShortInput(t *testing.T) {
	aesKey := make([]byte, 32)
	if _, err := DecryptDocument([]byte("short"), aesKey); err == nil {
		t.Fatal("expected error for ciphertext shorter than the IV")
	}
}

func TestDecryptDocument_WrongKeySize(t *testing.T) {
	if _, err := DecryptDocument(make([]byte, 64), make([]byte, 16)); err == nil {
		t.Fatal("expected error for a 16-byte (non AES-256) key")
	}
}

// TestDecryptAESKey_RoundTrip wraps a key with RSA-OAEP/SHA-256 (the
// sender's scheme) and confirms DecryptAESKey unwraps it.
func TestDecryptAESKey_RoundTrip(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pk := &printerKey{rsa: priv}

	aesKey := make([]byte, 32)
	rand.Read(aesKey)
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &priv.PublicKey, aesKey, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := pk.DecryptAESKey(wrapped)
	if err != nil {
		t.Fatalf("DecryptAESKey: %v", err)
	}
	if !bytes.Equal(got, aesKey) {
		t.Fatalf("unwrapped key mismatch")
	}
}

func TestPKCS7Unpad(t *testing.T) {
	tests := []struct {
		name    string
		in      []byte
		want    []byte
		wantErr bool
	}{
		{"one byte of pad", []byte{'a', 'b', 'c', 0x01}, []byte{'a', 'b', 'c'}, false},
		{"full block of pad", bytes.Repeat([]byte{0x04}, 4), []byte{}, false},
		{"zero pad invalid", []byte{'a', 'b', 'c', 0x00}, nil, true},
		{"pad longer than block", []byte{'a', 'b', 'c', 0x05}, nil, true},
		{"inconsistent pad", []byte{'a', 'b', 0x01, 0x02}, nil, true},
		{"not block aligned", []byte{'a', 'b', 'c'}, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pkcs7Unpad(tc.in, 4)
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

// TestPBKDF2_SHA256_Vectors checks the hand-rolled KDF against published
// PBKDF2-HMAC-SHA256 test vectors, independently of the ASN.1 path.
func TestPBKDF2_SHA256_Vectors(t *testing.T) {
	tests := []struct {
		password, salt string
		iter, keyLen   int
		wantHex        string
	}{
		{"password", "salt", 1, 32, "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{"password", "salt", 4096, 32, "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
	}
	for _, tc := range tests {
		got := pbkdf2Key(sha256.New, []byte(tc.password), []byte(tc.salt), tc.iter, tc.keyLen)
		if hex.EncodeToString(got) != tc.wantHex {
			t.Errorf("pbkdf2(%q,%q,%d): got %x, want %s", tc.password, tc.salt, tc.iter, got, tc.wantHex)
		}
	}
}

// TestLoadPrinterPrivateKey_PureGo builds an encrypted PKCS#8 key in-test
// (no external tools) and confirms the loader recovers the RSA key. This
// exercises the ASN.1 parse + PBKDF2 + AES-CBC + PKCS#7 path even where
// openssl is unavailable.
func TestLoadPrinterPrivateKey_PureGo(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	passphrase := []byte("correct horse battery staple")
	pemBytes := encryptPKCS8ForTest(t, priv, passphrase, 2048)

	loaded, err := loadPrinterPrivateKey(pemBytes, passphrase)
	if err != nil {
		t.Fatalf("loadPrinterPrivateKey: %v", err)
	}
	assertSameRSAKey(t, loaded, priv)

	// Wrong passphrase must fail (padding/parse error), not silently succeed.
	if _, err := loadPrinterPrivateKey(pemBytes, []byte("wrong passphrase")); err == nil {
		t.Fatal("expected failure with the wrong passphrase")
	}
}

// TestLoadPrinterPrivateKey_OpenSSLInterop is the real-world cross-check:
// it decrypts a key produced by the exact command gen-printer-keys.sh runs.
// Skipped where openssl is not installed.
func TestLoadPrinterPrivateKey_OpenSSLInterop(t *testing.T) {
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl not on PATH; skipping interop check")
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "printer-private.pem")
	const passphrase = "interop-passphrase"

	// Mirror gen-printer-keys.sh (2048 bits here for test speed).
	cmd := exec.Command(openssl, "genpkey", "-algorithm", "RSA",
		"-pkeyopt", "rsa_keygen_bits:2048",
		"-aes-256-cbc", "-pass", "pass:"+passphrase, "-out", keyPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("openssl genpkey: %v\n%s", err, out)
	}

	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pemBytes, []byte("ENCRYPTED PRIVATE KEY")) {
		t.Fatalf("expected an ENCRYPTED PRIVATE KEY PEM, got:\n%s", pemBytes)
	}

	loaded, err := loadPrinterPrivateKey(pemBytes, []byte(passphrase))
	if err != nil {
		t.Fatalf("loadPrinterPrivateKey on openssl key: %v", err)
	}
	// Prove the recovered key is usable: OAEP round-trip through it.
	aesKey := make([]byte, 32)
	rand.Read(aesKey)
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &loaded.rsa.PublicKey, aesKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	unwrapped, err := loaded.DecryptAESKey(wrapped)
	if err != nil {
		t.Fatalf("DecryptAESKey with openssl-loaded key: %v", err)
	}
	if !bytes.Equal(unwrapped, aesKey) {
		t.Fatal("round-trip through openssl-loaded key failed")
	}
}

// --- test helpers ---

// sealGCM produces the sender's wire format: [12-byte IV || GCM ct+tag].
func sealGCM(t *testing.T, aesKey, plaintext []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	sealed := gcm.Seal(nil, iv, plaintext, nil)
	return append(append([]byte{}, iv...), sealed...)
}

func assertSameRSAKey(t *testing.T, loaded *printerKey, want *rsa.PrivateKey) {
	t.Helper()
	if loaded.rsa.N.Cmp(want.N) != 0 || loaded.rsa.D.Cmp(want.D) != 0 {
		t.Fatal("loaded RSA key does not match the original")
	}
}

// encryptPKCS8ForTest wraps an RSA key as an encrypted PKCS#8 PEM using the
// same PBES2 shape openssl emits (PBKDF2-HMAC-SHA256 + AES-256-CBC), so the
// loader's decrypt path can be tested without invoking openssl. The struct
// tags omit the optional PBKDF2 keyLength, matching openssl.
func encryptPKCS8ForTest(t *testing.T, priv *rsa.PrivateKey, passphrase []byte, iter int) []byte {
	t.Helper()

	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	salt := make([]byte, 16)
	iv := make([]byte, aes.BlockSize)
	rand.Read(salt)
	rand.Read(iv)

	dk := pbkdf2Key(sha256.New, passphrase, salt, iter, 32)
	block, err := aes.NewCipher(dk)
	if err != nil {
		t.Fatal(err)
	}
	padded := pkcs7Pad(pkcs8, aes.BlockSize)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)

	type testPBKDF2Params struct {
		Salt       []byte
		Iterations int
		PRF        algorithmIdentifier
	}
	kdfParams, err := asn1.Marshal(testPBKDF2Params{
		Salt:       salt,
		Iterations: iter,
		PRF:        algorithmIdentifier{Algorithm: oidHMACWithSHA256, Parameters: asn1.NullRawValue},
	})
	if err != nil {
		t.Fatal(err)
	}
	ivDER, err := asn1.Marshal(iv)
	if err != nil {
		t.Fatal(err)
	}
	pbes2, err := asn1.Marshal(pbes2Params{
		KDF: algorithmIdentifier{Algorithm: oidPBKDF2, Parameters: asn1.RawValue{FullBytes: kdfParams}},
		Enc: algorithmIdentifier{Algorithm: oidAES256CBC, Parameters: asn1.RawValue{FullBytes: ivDER}},
	})
	if err != nil {
		t.Fatal(err)
	}
	der, err := asn1.Marshal(encryptedPrivateKeyInfo{
		Algo:          algorithmIdentifier{Algorithm: oidPBES2, Parameters: asn1.RawValue{FullBytes: pbes2}},
		EncryptedData: ct,
	})
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: der})
}

func pkcs7Pad(b []byte, blockSize int) []byte {
	pad := blockSize - len(b)%blockSize
	out := make([]byte, len(b)+pad)
	copy(out, b)
	for i := len(b); i < len(out); i++ {
		out[i] = byte(pad)
	}
	return out
}
