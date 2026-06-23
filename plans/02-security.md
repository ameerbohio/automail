# Security

## Threat Model Summary

| Threat | Mitigation |
|---|---|
| Cloud server compromise | Zero-knowledge design — server stores ciphertext only; no plaintext recoverable |
| Database exfiltration | PII fields encrypted with pgcrypto; encrypted_key column is ciphertext |
| Network interception (external) | TLS 1.3 via Traefik; HSTS enforced |
| Network interception (internal) | mTLS between cloud server and printer microservice |
| Plaintext persistence on printer | tmpfs only; immediate unlink after CUPS accepts job; in-memory zeroing |
| Unauthorized job dispatch | Printer only accepts mTLS connections from cloud server (verified by cert) |
| Brute force / abuse | Rate limiting via Traefik middleware; JWT expiry |
| Secrets exposure | `.env` excluded from git; Docker secrets pattern for production |

---

## 1. End-to-End Encryption (E2EE)

### Key Generation (browser)

```
1. Generate AES-256-GCM key:
   const aesKey = await crypto.subtle.generateKey(
     { name: 'AES-GCM', length: 256 }, true, ['encrypt', 'decrypt']
   )

2. Encrypt PDF:
   const iv = crypto.getRandomValues(new Uint8Array(12))
   const ciphertext = await crypto.subtle.encrypt(
     { name: 'AES-GCM', iv }, aesKey, pdfArrayBuffer
   )

3. Export raw AES key:
   const rawAesKey = await crypto.subtle.exportKey('raw', aesKey)

4. Fetch printer RSA public key from cloud server (PEM → import):
   const printerPubKey = await crypto.subtle.importKey(
     'spki', pemToDer(pubKeyPem), { name: 'RSA-OAEP', hash: 'SHA-256' },
     false, ['wrapKey']
   )

5. Wrap (encrypt) AES key with printer's RSA public key:
   const encryptedAesKey = await crypto.subtle.wrapKey(
     'raw', aesKey, printerPubKey, { name: 'RSA-OAEP' }
   )
```

The AES key and plaintext PDF never leave the browser. Only `ciphertext` (the encrypted PDF) and `encryptedAesKey` (the AES key encrypted with the printer's RSA public key) are sent to the server.

### Key Encapsulation Scheme

- **Document encryption**: AES-256-GCM with a random 12-byte IV, prepended to the ciphertext blob
- **Key encapsulation**: RSA-OAEP with SHA-256, 4096-bit RSA key per printer unit
- **Cloud server receives**: `encryptedAesKey` (opaque bytes), `blob_ref` (MinIO object path), `slot_id`, `page_count`
- **Cloud server stores**: exactly what it received — it cannot decrypt either field

### Zero-Knowledge Guarantee

The cloud server's stored record for a job:

```
job_id           → UUID (not sensitive)
encrypted_key    → RSA-OAEP ciphertext (cannot be decrypted without printer's private key)
blob_ref         → MinIO object key (ciphertext blob, cannot be decrypted without AES key)
slot_id          → mailbox slot identifier (metadata, not document content)
page_count       → integer (metadata)
status           → lifecycle state
```

Even with full read access to the database and MinIO, an attacker cannot recover the plaintext document.

---

## 2. Printer Key Management

### Key Generation

Each printer microservice instance holds an RSA-4096 keypair. For the prototype, generate with:

```bash
# Generate private key (encrypted at rest with AES-256)
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:4096 \
  -aes-256-cbc -out printer-private.pem

# Extract public key
openssl rsa -pubout -in printer-private.pem -out printer-public.pem
```

The private key file is loaded by the printer microservice at startup using the passphrase from an environment variable. The passphrase is never hardcoded.

### Key Registration

The mailbox's public key is registered with the cloud server out-of-band during mailbox setup — either via a CLI tool or by inserting directly into the `mailboxes` table. The cloud server serves the public key via `GET /mailboxes/:id/public-key` to authenticated senders.

### Private Key Handling in Go

```go
// Load and decrypt private key at startup
privKeyPEM, _ := os.ReadFile(os.Getenv("PRINTER_PRIVATE_KEY_PATH"))
privKeyPassphrase := []byte(os.Getenv("PRINTER_KEY_PASSPHRASE"))
block, _ := pem.Decode(privKeyPEM)
decryptedDER, _ := x509.DecryptPEMBlock(block, privKeyPassphrase)
privKey, _ := x509.ParsePKCS8PrivateKey(decryptedDER)

// Zero the passphrase after use
for i := range privKeyPassphrase { privKeyPassphrase[i] = 0 }
```

The private key object lives in memory for the lifetime of the process. It is never written to a temp file or logged.

---

## 3. Secure Wipe (Printer Microservice)

After printing is confirmed by CUPS, all decrypted content is erased:

```go
// Zero all sensitive byte slices
for i := range decryptedPDF   { decryptedPDF[i] = 0 }
for i := range rawAESKey      { rawAESKey[i] = 0 }
for i := range ciphertextBlob { ciphertextBlob[i] = 0 }
runtime.GC()

// Unlink tmpfs file (was never on a persistent disk)
os.Remove(tmpfsPath)
```

**tmpfs usage**: The decrypted PDF is written to `/dev/shm/automail-<job_id>.pdf` (an in-memory filesystem in Linux). This file never touches a persistent disk. It is unlinked immediately after CUPS accepts the job.

**Dev mode**: In development, the file is written to the system temp directory and deleted immediately. A log line confirms deletion.

---

## 4. mTLS (Cloud Server ↔ Printer Microservice)

Internal communication is protected by mutual TLS. Both sides present certificates signed by an internal CA.

### Certificate Generation (openssl scripts)

```bash
# 1. Generate internal CA
openssl req -x509 -newkey rsa:4096 -keyout ca-key.pem -out ca-cert.pem \
  -days 3650 -nodes -subj "/CN=automail-internal-ca"

# 2. Cloud server certificate
openssl req -newkey rsa:2048 -keyout cloud-key.pem -out cloud-csr.pem \
  -nodes -subj "/CN=cloud-server"
openssl x509 -req -in cloud-csr.pem -CA ca-cert.pem -CAkey ca-key.pem \
  -CAcreateserial -out cloud-cert.pem -days 365

# 3. Printer service certificate
openssl req -newkey rsa:2048 -keyout printer-key.pem -out printer-csr.pem \
  -nodes -subj "/CN=printer-service"
openssl x509 -req -in printer-csr.pem -CA ca-cert.pem -CAkey ca-key.pem \
  -CAcreateserial -out printer-cert.pem -days 365
```

The printer microservice verifies that incoming connections present a certificate signed by the internal CA. Only the cloud server has such a certificate, so no other caller can reach the dispatch endpoint.

---

## 5. Encryption at Rest

### MinIO (SSE-S3)

MinIO is configured with server-side encryption (SSE-S3). This adds a second encryption layer on top of the client-side E2EE — the blob is encrypted twice: once by the browser (AES-256-GCM) and once by MinIO at rest.

### PostgreSQL (pgcrypto)

PII fields are encrypted at the application layer before insertion:

```sql
-- Schema example
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Encrypted fields stored as bytea
email_enc BYTEA NOT NULL,  -- pgp_sym_encrypt(email, app_key)
name_enc  BYTEA NOT NULL
```

```go
// Go: encrypt before insert
encryptedEmail, _ = db.QueryRow(
  "SELECT pgp_sym_encrypt($1, $2)", email, appKey
).Scan(&encryptedEmail)

// Go: decrypt on read
db.QueryRow("SELECT pgp_sym_decrypt(email_enc, $1) FROM senders WHERE id = $2", appKey, id)
```

The `appKey` is a secret injected at runtime via environment variable, never stored in the database.

---

## 6. Transport Security (Traefik)

Traefik static config (`traefik.yml`):

```yaml
tls:
  options:
    default:
      minVersion: VersionTLS13
      sniStrict: true

# Dynamic config (labels or file provider)
# HSTS: Strict-Transport-Security: max-age=31536000; includeSubDomains
# CSP: Content-Security-Policy: default-src 'self'
# X-Frame-Options: DENY
# X-Content-Type-Options: nosniff
# Referrer-Policy: no-referrer
```

Rate limiting middleware: 20 requests/min per IP on upload and job submission endpoints.

---

## 7. Authentication (JWT)

- **Algorithm**: RS256 (asymmetric — public key can be distributed to multiple cloud server nodes without sharing the signing secret)
- **Access token**: 15-minute expiry, signed with server's RSA private key
- **Refresh token**: 7-day expiry, stored as a hash in the `refresh_tokens` table, set as httpOnly + Secure + SameSite=Strict cookie
- **Rotation**: refresh token is single-use; a new one is issued on each `/auth/refresh` call; old token is immediately invalidated
- **Logout**: refresh token hash is deleted from the database; access token expires naturally (short-lived enough to not warrant a blocklist for the prototype)

---

## 8. Audit Log

Every significant event is appended to the `audit_events` table:

| Action | Trigger |
|---|---|
| `job_submitted` | Sender POST /jobs |
| `job_dispatched` | Cloud server → printer dispatch |
| `job_printing` | Printer status callback |
| `job_delivered` | Printer confirms delivery |
| `blob_deleted` | MinIO object deleted |
| `job_failed` | Printer or dispatch failure |

A PostgreSQL trigger prevents UPDATE or DELETE on this table:

```sql
CREATE OR REPLACE FUNCTION audit_no_mutate() RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION 'audit_events rows are immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_immutable
  BEFORE UPDATE OR DELETE ON audit_events
  FOR EACH ROW EXECUTE FUNCTION audit_no_mutate();
```

---

## 9. Secrets Management

For the prototype, secrets are injected via a `.env` file (excluded from git via `.gitignore`). An `.env.example` file documents all required variables with placeholder values.

```
POSTGRES_PASSWORD=
REDIS_PASSWORD=
MINIO_ROOT_USER=
MINIO_ROOT_PASSWORD=
JWT_PRIVATE_KEY_PATH=
JWT_PUBLIC_KEY_PATH=
APP_ENCRYPTION_KEY=
PRINTER_PRIVATE_KEY_PATH=
PRINTER_KEY_PASSPHRASE=
MTLS_CA_CERT_PATH=
MTLS_CLOUD_CERT_PATH=
MTLS_CLOUD_KEY_PATH=
MTLS_PRINTER_CERT_PATH=
MTLS_PRINTER_KEY_PATH=
```

Production path: Docker secrets (`docker secret create`) — secrets are mounted as files into containers, not passed as environment variables.
