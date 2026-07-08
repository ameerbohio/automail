// In-browser hybrid encryption. Runs entirely client-side using Web Crypto
// (window.crypto.subtle). No plaintext PDF and no raw AES key ever leaves the
// browser -- the cloud server is zero-knowledge (plans/02-security.md,
// plans/06-sender-portal.md).
//
// This is the *encrypt* half of the contract the printer's Phase 6 decrypt
// side enforces (services/printer/crypto.go, docs/study/16-hybrid-encryption.md).
// It must produce exactly what the printer expects:
//
//   - blob = [ 12-byte IV || AES-256-GCM(ciphertext + 16-byte tag) ], no AAD.
//     The IV is prepended; the printer reads it from the first 12 bytes.
//   - AES key wrap = RSA-OAEP with SHA-256 digest + MGF1(SHA-256), empty label,
//     using the recipient printer's RSA-4096 public key (SPKI PEM).
//   - encrypted_key travels to the cloud as standard-base64 (the cloud server
//     base64.StdEncoding.DecodeString's it in handlers/jobs.go).

export interface EncryptedJob {
  /** [12-byte IV || AES-256-GCM ciphertext+tag] -- uploaded to MinIO as-is. */
  ciphertext: ArrayBuffer;
  /** RSA-OAEP-wrapped AES content key -- sent as encrypted_key (base64). */
  encryptedKey: ArrayBuffer;
}

// pemToDer strips the PEM armor and base64-decodes the body to the raw DER
// bytes that importKey('spki', ...) expects.
function pemToDer(pem: string): ArrayBuffer {
  const body = pem
    .replace(/-----BEGIN [^-]+-----/, "")
    .replace(/-----END [^-]+-----/, "")
    .replace(/\s+/g, "");
  const binary = atob(body);
  const der = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    der[i] = binary.charCodeAt(i);
  }
  return der.buffer as ArrayBuffer;
}

export async function encryptDocument(
  pdfBuffer: ArrayBuffer,
  printerPublicKeyPem: string,
): Promise<EncryptedJob> {
  // 1. One-time AES-256-GCM content key. extractable:true so step 4 can wrap
  //    it; the raw key is never exported to JS or sent anywhere.
  const aesKey = await crypto.subtle.generateKey(
    { name: "AES-GCM", length: 256 },
    true,
    ["encrypt"],
  );

  // 2. Encrypt the PDF under a fresh 96-bit IV (the GCM standard nonce size).
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const encrypted = await crypto.subtle.encrypt(
    { name: "AES-GCM", iv },
    aesKey,
    pdfBuffer,
  );

  // Prepend the IV: [12-byte IV | ciphertext+tag]. DecryptDocument on the
  // printer slices the first 12 bytes back off as the nonce.
  const blob = new Uint8Array(12 + encrypted.byteLength);
  blob.set(iv, 0);
  blob.set(new Uint8Array(encrypted), 12);

  // 3. Import the printer's RSA public key (SubjectPublicKeyInfo / SPKI PEM).
  const printerPubKey = await crypto.subtle.importKey(
    "spki",
    pemToDer(printerPublicKeyPem),
    { name: "RSA-OAEP", hash: "SHA-256" },
    false,
    ["wrapKey"],
  );

  // 4. Wrap the AES key with RSA-OAEP. 32 bytes of raw key material in, RSA
  //    ciphertext out; the printer RSA-OAEP-unwraps it (DecryptAESKey).
  const encryptedKey = await crypto.subtle.wrapKey(
    "raw",
    aesKey,
    printerPubKey,
    { name: "RSA-OAEP" },
  );

  return { ciphertext: blob.buffer as ArrayBuffer, encryptedKey };
}

// bufferToBase64 encodes bytes as *standard* base64 (not URL-safe): the cloud
// server decodes encrypted_key with base64.StdEncoding (handlers/jobs.go).
// Chunked to stay under the argument-count limit of String.fromCharCode.
export function bufferToBase64(buf: ArrayBuffer): string {
  const bytes = new Uint8Array(buf);
  let binary = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(binary);
}
