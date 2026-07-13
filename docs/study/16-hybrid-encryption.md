# Hybrid Encryption: RSA-OAEP + AES-256-GCM

**What it is.** The sender encrypts the PDF with a randomly generated AES-256-GCM key (symmetric, fast, no size limit). That AES key is then encrypted with the printer's RSA-OAEP public key. Both blobs travel to the cloud. The printer uses its RSA private key to unwrap the AES key, then uses the AES key to decrypt the document.

**Why not RSA alone.** RSA has a hard size ceiling — it can only encrypt data *smaller than the key size*. A 4096-bit RSA key can encrypt at most ~446 bytes of payload (after OAEP padding). A PDF blows past that immediately. So RSA encrypts only the small AES key (~32 bytes); AES handles the bulk document with no size limit.

**Why not AES alone.** AES is symmetric — both sides need the same secret key. But the sender and printer have never met; there's no secure channel to share a key before the job arrives. RSA's asymmetric keypair solves this: the sender encrypts to the printer's *public* key, and only the printer's *private* key can undo it. No pre-shared secret needed.

**Why GCM over CBC.** GCM is AEAD (Authenticated Encryption with Associated Data) — it detects tampering. CBC only hides data; a ciphertext can be silently flipped without detection.

**Why OAEP over textbook RSA.** OAEP adds randomised padding, so the same plaintext gives a different ciphertext each time. Textbook RSA is deterministic, enabling the Bleichenbacher padding-oracle attack.

**Why 4096-bit RSA.** Mailbox keys don't rotate frequently. A compromise of a 2048-bit key would expose all historical jobs for that printer; 4096-bit raises the break cost enough to justify the one-time keygen overhead.

**The honest caveat.** If the printer's RSA private key is extracted from the device (physical attack), all past jobs encrypted to that key are retrospectively compromised. Production would need key rotation + forward secrecy (e.g. ephemeral ECDH per job).

---

## Decryption inside the printer (Phase 6)

**Plaintext lives in two places, both transient.** The unwrapped AES key and the decrypted PDF exist only as Go byte slices in RAM and as one file on `/dev/shm` — tmpfs, a RAM-backed filesystem, never the physical disk. No plaintext is ever written to real disk, a log line, or the network.

**Unlink before you report success.** The tmpfs file is `os.Remove`d *before* the printer sends the `delivered` status frame — not in a `defer`. If the unlink fails, the job is reported `failed`, never `delivered`: a reported success must never leave plaintext behind. Ordering *is* the guarantee, so it cannot be deferred to "eventually."

**Zero, drop the reference, then hint the GC.** After printing, every sensitive slice (ciphertext, wrapped key, raw AES key, plaintext PDF) is overwritten with zeros in place, the local references are dropped, and `runtime.GC()` is called. The zeroing is what matters — it scrubs the backing array immediately. Go's GC gives no timing guarantee, so we never rely on *collection* to remove a secret, only on having *overwritten* it first.

**Failures must not become an oracle.** Every decrypt failure — RSA unwrap, GCM authentication, base64 decode — goes back up the wire as the same generic `"processing failed"`. Telling the sender *which* stage failed (bad padding vs. bad auth tag) is exactly the bit a padding/decryption oracle needs. The specific cause is logged locally only, and never carries document bytes.

## Loading the printer's private key

**The key file is an encrypted PKCS#8 blob, and Go's stdlib can't open it.** `openssl genpkey -aes-256-cbc` writes a PEM of type `ENCRYPTED PRIVATE KEY` in **PBES2** format (RFC 8018): the RSA key is wrapped with AES-256-CBC and that AES key is derived from the passphrase with **PBKDF2**. `crypto/x509` only parses *unencrypted* PKCS#8, so the printer walks the ASN.1 itself — reads the PBKDF2 salt / iteration count / PRF and the AES-CBC IV out of the structure, derives the key, undoes the CBC + PKCS#7 padding, then parses the now-plaintext PKCS#8.

**PBKDF2 is hand-rolled over stdlib HMAC — deliberately.** `crypto/pbkdf2.Key` only accepts the password as a Go `string`, which is immutable and can never be wiped. Rolling the ~15-line PBKDF2 loop over `crypto/hmac` lets the passphrase stay a `[]byte` the printer is free to zero, and keeps the microservice dependency-free ("intentionally minimal, easy to audit"). It's checked against published PBKDF2-HMAC-SHA256 vectors *and* against a key produced by the real `openssl` command.

**Passphrase hygiene, and its honest limit.** The passphrase is read from `PRINTER_KEY_PASSPHRASE`; the env var is immediately `os.Unsetenv`'d (so it can't be read via `/proc/self/environ` or inherited by child processes), and the `[]byte` copy used for derivation is zeroed right after. The *RSA private key itself* stays resident in RAM for the process lifetime — it must, to decrypt every job — so only the passphrase, whose job ends after one key load, is scrubbed. The unavoidable gap: `os.Getenv` returns a Go `string`, which can't be zeroed and lingers until GC. Prototype-acceptable; production would source the passphrase into locked, mutable memory (`memguard`, OS-locked pages, or an HSM that never exposes the key at all).

## Deleting the ciphertext after delivery

**A delivered job's ciphertext is deleted from object storage.** Once the printer reports `delivered`, the cloud hub calls MinIO `RemoveObject` on the blob and stamps `blob_deleted_at`. The ciphertext has already been decrypted and printed; keeping it only widens the window in which it could be exfiltrated (data minimization). The delete runs *after* the SSE status publish — a slow delete must never stall the sender's update — and is keyed on `blob_ref`, so the zero-knowledge boundary holds: `encrypted_key` is never read on this path. A failed delete is logged and swallowed; a leftover blob is a hygiene issue for a TTL/sweep, not a correctness bug.

---

## Pending side questions (answer before interview)

- **Why can't we just use AES directly?** (key distribution problem — how do sender and printer share the key without a pre-existing secure channel?)
