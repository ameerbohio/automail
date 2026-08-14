# Security & cryptography — at a glance

*Deep versions: [hybrid encryption](../docs/study/16-hybrid-encryption.md), [browser E2EE](../docs/study/18-web-crypto-e2ee-portal.md), [internal mTLS PKI](../docs/study/05-mtls-internal-pki.md). Spec: [plans/02-security.md](../plans/02-security.md).*

**The claim: the operator of this system cannot read the mail it delivers.** Not "does not" —
*cannot*. A full database dump plus every object in blob storage yields ciphertext and metadata.

Every row and bullet below carries a **see** pointer to the file that proves it.

## How that is enforced

| Property | Mechanism | See |
|---|---|---|
| **The cloud never sees plaintext** | The browser encrypts with AES-256-GCM and wraps that key to the mailbox's RSA-4096 public key (OAEP). The server stores the wrapped key verbatim and forwards it to exactly one place. | the encryption itself: [`encrypt.ts` → `encryptDocument`](../services/portal/lib/encrypt.ts#L39) · the guard: [`TestInvariant_ZeroKnowledgeCloud`](../services/cloud/security_invariants_test.go#L249) — **a failing build**, not a code comment |
| **The cloud never even reads the ciphertext** | Uploads and downloads go direct to object storage via pre-signed URLs; the server hands out capabilities, never bytes. | the only permitted MinIO surface: [`minioclient/`](../services/cloud/minioclient/) · the guard: [`TestInvariant_CloudNeverStreamsBlobBytes`](../services/cloud/blob_readpath_test.go#L60) |
| **Plaintext exists only in RAM** | The printer decrypts to `/dev/shm` (tmpfs — never on a disk), unlinks the file **before** reporting delivery, then zeroes the buffers. | [`tmpfsDir = "/dev/shm"`](../services/printer/print.go#L73) · [the unlink, ahead of the status callback](../services/printer/print.go#L199) · [`zeroBytes`](../services/printer/crypto.go#L66) · the guards: [`TestInvariant_TmpfsDirUnderDevShm`](../services/printer/security_invariants_test.go#L26), [`TestInvariant_PlaintextWritesTargetTmpfsOnly`](../services/printer/security_invariants_test.go#L98) |
| **Key material is not left lying around** | The printer's key passphrase is removed from the environment once the key is loaded. | [`main.go` → `os.Unsetenv("PRINTER_KEY_PASSPHRASE")`](../services/printer/main.go#L55) · the guard: [`TestInvariant_PassphraseEnvUnsetAfterLoad`](../services/printer/security_invariants_test.go#L143) |
| **Every internal hop is mutually authenticated** | Private CA; the printer dials *out* over an mTLS WebSocket so a mailbox unit never needs an inbound firewall hole. | [the CA and cert generators](../infra/certs/) · [the printer's client-side TLS config](../services/printer/mtls.go#L16) · [the dial-out endpoint it connects to](../services/cloud/handlers/printerlink.go#L15) · the guard: [`TestInvariant_InternalListenerRequiresClientCert`](../services/cloud/security_invariants_test.go#L106) — a certless client is **refused** |

**The detail worth noticing:** several of those guards have a sibling that deliberately *violates*
the invariant and asserts the guard trips. A test that only ever passes proves the guard runs, not
that it works. **see** [`TestInvariant_ScannerCatchesViolations`](../services/cloud/security_invariants_test.go#L258) ·
[`TestInvariant_BlobByteScannerCatchesViolation`](../services/cloud/blob_readpath_test.go#L87) ·
[`TestInvariant_ScannerCatchesOffTmpfsWrite`](../services/printer/security_invariants_test.go#L125)

## The cross-language problem, solved with a test

The encrypting code is TypeScript running in a browser; the decrypting code is Go running on a
printer. Testing each against OpenSSL proves neither actually agrees with **the other**. So a
committed contract test has the portal emit a real `{encrypted_key, iv, ciphertext}` vector and the
Go printer decrypt it **byte-for-byte** — plus a one-bit-flip case asserting the tamper is rejected
outright, with no partial plaintext. It runs in CI on every push.

**see** the browser half: [`crypto-contract.contract.test.ts`](../services/portal/lib/crypto-contract.contract.test.ts) ·
the Go half: [`TestContractPrinterDecryptsBrowser`](../services/printer/crypto_contract_test.go#L71) and
[`TestContractGoEncryptForBrowser`](../services/printer/crypto_contract_test.go#L121) ·
the committed vectors: [`testdata/crypto-contract/`](../testdata/crypto-contract/) ·
the three-step runner: [`make crypto-contract`](../Makefile#L63) ·
in CI: [the `crypto-contract` job](../.github/workflows/ci.yml#L81)

## Supply chain

`govulncheck` (Go CVEs with reachability analysis) · `gosec` (SAST, documented excludes) ·
`gitleaks` (secrets across git history) · `npm audit`. All wired into CI as gates. Findings
deliberately *accepted* rather than fixed are written down with re-review triggers — an unfixed
finding with a stated reason and an expiry beats a silently ignored one.

**see** [`make scan`, with each exclude justified inline](../Makefile#L72) ·
[the `security` CI job](../.github/workflows/ci.yml#L101) ·
[`.gitleaks.toml`](../.gitleaks.toml) (allowlisted test fixtures) ·
[`docs/accepted-risks.md`](../docs/accepted-risks.md)

## What this is not

| Limit | See |
|---|---|
| Self-signed internal PKI — no cert-manager, no HSM | [the shell scripts that are the whole PKI](../infra/certs/gen.sh) |
| No service mesh, no NetworkPolicy | [the manifests, which contain neither](../infra/k8s/base/) |
| Threat-modelled for an honest-but-curious operator, not one who has already compromised the running printer | [plans/02-security.md](../plans/02-security.md) |
| The physical unit's spool-to-disk behaviour under CUPS is an open question, not a solved one | [the accepted risk](../docs/accepted-risks.md) · [the open question](../docs/study/00-interview-pending-questions.md) · [the real-print path it applies to](../services/printer/print.go) |
