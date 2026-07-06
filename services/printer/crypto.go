package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
)

// printerKey wraps the RSA private key the printer uses to unwrap the AES
// document key on every job. It lives in RAM for the life of the process
// (it must, to decrypt jobs); only the passphrase that unlocked it is
// zeroed after load (see loadDocKey in main.go).
type printerKey struct {
	rsa *rsa.PrivateKey
}

// DecryptAESKey RSA-OAEP-unwraps the per-job AES key the sender wrapped
// with this printer's public key (plans/06-sender-portal.md wraps with
// RSA-OAEP + SHA-256, empty label; plans/04 step 2). SHA-256 here must
// match the sender's hash, and the label (nil) must match the sender's
// empty label -- a mismatch decodes to garbage or an error.
func (k *printerKey) DecryptAESKey(encryptedKey []byte) ([]byte, error) {
	return rsa.DecryptOAEP(sha256.New(), rand.Reader, k.rsa, encryptedKey, nil)
}

// DecryptDocument AES-256-GCM-decrypts the PDF. The wire format is
// [12-byte IV || ciphertext+tag] with no AAD (plans/06-sender-portal.md
// "Prepend IV to ciphertext"; plans/04 steps 3-4). GCM authenticates the
// ciphertext, so a wrong key or any tampering surfaces as an Open error
// rather than silently wrong plaintext.
func DecryptDocument(ciphertext, aesKey []byte) ([]byte, error) {
	if len(aesKey) != 32 {
		return nil, fmt.Errorf("aes key is %d bytes, want 32 (AES-256)", len(aesKey))
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block) // default 12-byte nonce, matches the sender's IV
	if err != nil {
		return nil, err
	}
	ivLen := gcm.NonceSize()
	if len(ciphertext) < ivLen {
		return nil, fmt.Errorf("ciphertext is %d bytes, shorter than the %d-byte IV", len(ciphertext), ivLen)
	}
	iv, ct := ciphertext[:ivLen], ciphertext[ivLen:]
	return gcm.Open(nil, iv, ct, nil)
}

// zeroBytes overwrites a sensitive slice in place. Go's GC gives no
// timing guarantee, but zeroing removes the plaintext/key from the backing
// array immediately, and nil-ing the caller's reference drops it from the
// live set (plans/04 "In-Memory Zeroing Pattern").
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// loadPrinterPrivateKey parses the printer's document RSA private key from
// a PEM file. gen-printer-keys.sh produces an encrypted PKCS#8 key
// ("ENCRYPTED PRIVATE KEY", PBES2) via `openssl genpkey -aes-256-cbc`;
// Go's stdlib cannot decrypt PKCS#8, so decryptPBES2 does it (RFC 8018).
// An unencrypted "PRIVATE KEY" is accepted too, for completeness. The
// passphrase is the caller's to zero; this function additionally zeros the
// decrypted PKCS#8 DER (which contains the private key in the clear)
// before returning.
func loadPrinterPrivateKey(pemBytes, passphrase []byte) (*printerKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found in private key file")
	}

	var der []byte
	switch block.Type {
	case "ENCRYPTED PRIVATE KEY":
		d, err := decryptPBES2(block.Bytes, passphrase)
		if err != nil {
			return nil, fmt.Errorf("decrypt private key: %w", err)
		}
		der = d
		defer zeroBytes(der)
	case "PRIVATE KEY":
		der = block.Bytes
	default:
		return nil, fmt.Errorf("unexpected PEM type %q (want %q)", block.Type, "ENCRYPTED PRIVATE KEY")
	}

	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS#8 private key: %w", err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is %T, want *rsa.PrivateKey", parsed)
	}
	return &printerKey{rsa: rsaKey}, nil
}

// --- PBES2 (RFC 8018 / PKCS#5 v2.1) decryption of an encrypted PKCS#8 key ---
//
// The structure openssl emits is:
//
//   EncryptedPrivateKeyInfo ::= SEQUENCE {
//     encryptionAlgorithm  AlgorithmIdentifier (PBES2),
//     encryptedData        OCTET STRING }
//   PBES2-params ::= SEQUENCE { keyDerivationFunc (PBKDF2), encryptionScheme (AES-CBC) }
//   PBKDF2-params ::= SEQUENCE { salt OCTET STRING, iterationCount INTEGER,
//                                keyLength INTEGER OPTIONAL, prf AlgorithmIdentifier OPTIONAL }
//
// Go's crypto/x509 only parses *unencrypted* PKCS#8, so we walk this ASN.1
// by hand, derive the key with PBKDF2, and undo the AES-CBC + PKCS#7 pad.

var (
	oidPBES2          = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2         = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidHMACWithSHA1   = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 7}
	oidHMACWithSHA256 = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 9}
	oidHMACWithSHA512 = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 11}
	oidAES128CBC      = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 2}
	oidAES192CBC      = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 22}
	oidAES256CBC      = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
)

// prfHashes maps a PBKDF2 PRF OID to its hash constructor. openssl 3 uses
// hmacWithSHA256; RFC 8018 defaults to SHA-1 when the field is absent.
var prfHashes = map[string]func() hash.Hash{
	oidHMACWithSHA1.String():   sha1.New,
	oidHMACWithSHA256.String(): sha256.New,
	oidHMACWithSHA512.String(): sha512.New,
}

// aesCBCKeyLen maps an AES-CBC OID to its key length in bytes.
var aesCBCKeyLen = map[string]int{
	oidAES128CBC.String(): 16,
	oidAES192CBC.String(): 24,
	oidAES256CBC.String(): 32,
}

type algorithmIdentifier struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type encryptedPrivateKeyInfo struct {
	Algo          algorithmIdentifier
	EncryptedData []byte
}

type pbes2Params struct {
	KDF algorithmIdentifier
	Enc algorithmIdentifier
}

type pbkdf2Params struct {
	Salt       []byte
	Iterations int
	KeyLength  int                 `asn1:"optional"`
	PRF        algorithmIdentifier `asn1:"optional"`
}

func decryptPBES2(der, passphrase []byte) ([]byte, error) {
	var epki encryptedPrivateKeyInfo
	if _, err := asn1.Unmarshal(der, &epki); err != nil {
		return nil, fmt.Errorf("parse EncryptedPrivateKeyInfo: %w", err)
	}
	if !epki.Algo.Algorithm.Equal(oidPBES2) {
		return nil, fmt.Errorf("unsupported key encryption %v: only PBES2 is supported", epki.Algo.Algorithm)
	}

	var params pbes2Params
	if _, err := asn1.Unmarshal(epki.Algo.Parameters.FullBytes, &params); err != nil {
		return nil, fmt.Errorf("parse PBES2 parameters: %w", err)
	}

	// Key derivation must be PBKDF2.
	if !params.KDF.Algorithm.Equal(oidPBKDF2) {
		return nil, fmt.Errorf("unsupported KDF %v: only PBKDF2 is supported", params.KDF.Algorithm)
	}
	var kdf pbkdf2Params
	if _, err := asn1.Unmarshal(params.KDF.Parameters.FullBytes, &kdf); err != nil {
		return nil, fmt.Errorf("parse PBKDF2 parameters: %w", err)
	}
	prf := sha1.New // RFC 8018 default when prf is absent
	if len(kdf.PRF.Algorithm) > 0 {
		h, ok := prfHashes[kdf.PRF.Algorithm.String()]
		if !ok {
			return nil, fmt.Errorf("unsupported PBKDF2 PRF %v", kdf.PRF.Algorithm)
		}
		prf = h
	}

	// Encryption scheme must be AES-CBC; its OID fixes the key length.
	keyLen, ok := aesCBCKeyLen[params.Enc.Algorithm.String()]
	if !ok {
		return nil, fmt.Errorf("unsupported encryption scheme %v: only AES-CBC is supported", params.Enc.Algorithm)
	}
	if kdf.KeyLength > 0 { // honor an explicit PBKDF2 keyLength if present
		keyLen = kdf.KeyLength
	}
	var iv []byte
	if _, err := asn1.Unmarshal(params.Enc.Parameters.FullBytes, &iv); err != nil {
		return nil, fmt.Errorf("parse AES-CBC IV: %w", err)
	}
	if len(iv) != aes.BlockSize {
		return nil, fmt.Errorf("AES-CBC IV is %d bytes, want %d", len(iv), aes.BlockSize)
	}

	dk := pbkdf2Key(prf, passphrase, kdf.Salt, kdf.Iterations, keyLen)
	defer zeroBytes(dk)

	block, err := aes.NewCipher(dk)
	if err != nil {
		return nil, err
	}
	ct := epki.EncryptedData
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("AES-CBC ciphertext length %d is not a block multiple", len(ct))
	}
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ct)
	unpadded, err := pkcs7Unpad(plain, aes.BlockSize)
	if err != nil {
		zeroBytes(plain)
		return nil, err
	}
	return unpadded, nil
}

// pbkdf2Key is PBKDF2 (RFC 8018 §5.2): DK = T_1 || T_2 || ... where each
// T_i = U_1 XOR U_2 XOR ... XOR U_c, U_1 = PRF(P, salt || INT(i)), and
// U_j = PRF(P, U_{j-1}). PRF is HMAC over the chosen hash. Kept in-house
// (over stdlib crypto/hmac) so the printer takes no new dependency and can
// hold the passphrase as a []byte it is free to zero -- crypto/pbkdf2.Key
// only accepts a string, which cannot be wiped.
func pbkdf2Key(h func() hash.Hash, password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(h, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen

	dk := make([]byte, 0, numBlocks*hashLen)
	u := make([]byte, hashLen)
	t := make([]byte, hashLen)
	var blockIdx [4]byte
	for i := 1; i <= numBlocks; i++ {
		blockIdx[0] = byte(i >> 24)
		blockIdx[1] = byte(i >> 16)
		blockIdx[2] = byte(i >> 8)
		blockIdx[3] = byte(i)

		prf.Reset()
		prf.Write(salt)
		prf.Write(blockIdx[:])
		u = prf.Sum(u[:0])
		copy(t, u)

		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for x := range t {
				t[x] ^= u[x]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

// pkcs7Unpad strips PKCS#7 padding (RFC 5652 §6.3): the last byte is the
// pad length n, and the final n bytes must all equal n.
func pkcs7Unpad(b []byte, blockSize int) ([]byte, error) {
	n := len(b)
	if n == 0 || n%blockSize != 0 {
		return nil, errors.New("pkcs7: data is not a whole number of blocks")
	}
	pad := int(b[n-1])
	if pad == 0 || pad > blockSize || pad > n {
		return nil, errors.New("pkcs7: invalid padding length")
	}
	for _, c := range b[n-pad:] {
		if int(c) != pad {
			return nil, errors.New("pkcs7: inconsistent padding bytes")
		}
	}
	return b[:n-pad], nil
}
