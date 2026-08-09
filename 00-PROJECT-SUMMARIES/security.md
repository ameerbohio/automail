# Security & cryptography — at a glance

*Deep versions: [hybrid encryption](../docs/study/16-hybrid-encryption.md), [browser E2EE](../docs/study/18-web-crypto-e2ee-portal.md), [internal mTLS PKI](../docs/study/05-mtls-internal-pki.md). Spec: [plans/02-security.md](../plans/02-security.md).*

**The claim: the operator of this system cannot read the mail it delivers.** Not "does not" —
*cannot*. A full database dump plus every object in blob storage yields ciphertext and metadata.

## How that is enforced

| Property | Mechanism | Enforced by |
|---|---|---|
| **The cloud never sees plaintext** | The browser encrypts with AES-256-GCM and wraps that key to the mailbox's RSA-4096 public key (OAEP). The server stores the wrapped key verbatim and forwards it to exactly one place. | `TestInvariant_ZeroKnowledgeCloud` — **a failing build**, not a code comment |
| **The cloud never even reads the ciphertext** | Uploads and downloads go direct to object storage via pre-signed URLs; the server hands out capabilities, never bytes. | `TestInvariant_CloudNeverStreamsBlobBytes` |
| **Plaintext exists only in RAM** | The printer decrypts to `/dev/shm` (tmpfs — never on a disk), unlinks the file **before** reporting delivery, then zeroes the buffers. | `TestInvariant_PlaintextWritesTargetTmpfsOnly`, `TestInvariant_TmpfsDirUnderDevShm` |
| **Key material is not left lying around** | The printer's key passphrase is removed from the environment once the key is loaded. | `TestInvariant_PassphraseEnvUnsetAfterLoad` |
| **Every internal hop is mutually authenticated** | Private CA; the printer dials *out* over an mTLS WebSocket so a mailbox unit never needs an inbound firewall hole. | `TestInvariant_InternalListenerRequiresClientCert` — a certless client is **refused** |

**The detail worth noticing:** several of those tests have a sibling — `TestInvariant_ScannerCatchesViolation` —
that deliberately *violates* the invariant and asserts the guard trips. A test that only ever passes
proves the guard runs, not that it works.

## The cross-language problem, solved with a test

The encrypting code is TypeScript running in a browser; the decrypting code is Go running on a
printer. Testing each against OpenSSL proves neither actually agrees with **the other**. So a
committed contract test has the portal emit a real `{encrypted_key, iv, ciphertext}` vector and the
Go printer decrypt it **byte-for-byte** — plus a one-bit-flip case asserting the tamper is rejected
outright, with no partial plaintext. It runs in CI on every push (`make crypto-contract`).

## Supply chain

`govulncheck` (Go CVEs with reachability analysis) · `gosec` (SAST, documented excludes) ·
`gitleaks` (secrets across git history) · `npm audit`. All wired into CI as gates. Findings
deliberately *accepted* rather than fixed are written down with re-review triggers in
[accepted-risks.md](../docs/accepted-risks.md) — an unfixed finding with a stated reason and an expiry
beats a silently ignored one.

## What this is not

Self-signed internal PKI (no cert-manager or HSM), no service mesh or network policy, and
threat-modelled for an honest-but-curious operator rather than one who has already compromised the
running printer. The physical unit's spool-to-disk behaviour under CUPS is a documented open
question, not a solved one.

## Where to look

[`services/cloud/security_invariants_test.go`](../services/cloud/security_invariants_test.go) ·
[`services/printer/security_invariants_test.go`](../services/printer/security_invariants_test.go) ·
[`services/portal/lib/encrypt.ts`](../services/portal/lib/encrypt.ts) ·
[`services/printer/crypto.go`](../services/printer/crypto.go) ·
[`testdata/crypto-contract/`](../testdata/crypto-contract/) ·
[`infra/certs/`](../infra/certs/)
