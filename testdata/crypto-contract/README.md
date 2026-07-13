# Cross-language crypto contract fixtures (Testing Goal T2 / Part 3)

Proves the security-critical seam agrees byte-for-byte across languages:

- **Direction A (production path):** the browser encryptor
  (`services/portal/lib/encrypt.ts`, Web Crypto AES-256-GCM + RSA-OAEP/SHA-256)
  produces a job that the printer decryptor (`services/printer/crypto.go`,
  `DecryptAESKey` + `DecryptDocument`) reproduces exactly.
- **Direction B (guard):** Go encrypts the same way and the browser decrypts it,
  catching asymmetries even though only A ships.

Run it: `make crypto-contract` (from the repo root).

## Files

- `fixture.json` — **committed.** A NON-PRODUCTION RSA-4096 keypair (SPKI public +
  PKCS#8 private PEM strings). Safe to commit; no deployed service ever uses it. It
  is JSON rather than `*.pem` on purpose, so it is exempt from the `*.pem`
  gitignore and the pre-commit secret guard (both there to stop *real* keys
  leaking). Regenerate with:
  `openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:4096`.
- `*.vector.json` — **generated, gitignored.** The `{input, encrypted_key,
  ciphertext}` vectors are regenerated on every `make crypto-contract` run so the
  contract is re-proved, never stale-committed.
