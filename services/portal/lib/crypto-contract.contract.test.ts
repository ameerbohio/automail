// Cross-language crypto contract, browser side (Testing Goal T2 / Part 3).
//
// Named *.contract.test.ts so the default `vitest run` (unit tests, Goals T4/T6)
// skips it — it exchanges generated vector files with the printer and only runs
// under `make crypto-contract` (via vitest.contract.config.ts), which sequences
// the two languages.
//
//   Direction A (production path): encryptDocument() produces a vector the printer
//     decrypts byte-for-byte (verified by the Go side).
//   Direction B (guard): decrypt a Go-produced vector here, catching asymmetries.
import { describe, it, expect } from "vitest";
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve, join } from "node:path";
import { encryptDocument, bufferToBase64 } from "./encrypt";

const here = dirname(fileURLToPath(import.meta.url));
// services/portal/lib -> repo root is three levels up.
const contractDir = resolve(here, "../../../testdata/crypto-contract");

const fixture = JSON.parse(
  readFileSync(join(contractDir, "fixture.json"), "utf8"),
) as { public_spki_pem: string; private_pkcs8_pem: string };

function b64ToBytes(b64: string): Uint8Array {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function pemToDer(pem: string): ArrayBuffer {
  const body = pem
    .replace(/-----BEGIN [^-]+-----/, "")
    .replace(/-----END [^-]+-----/, "")
    .replace(/\s+/g, "");
  return b64ToBytes(body).buffer;
}

describe("crypto contract (browser <-> printer)", () => {
  // Direction A: encrypt with the real production path, emit the vector the
  // printer must reproduce. The byte-for-byte assertion lives on the Go side.
  it("browser encrypts a vector the printer can decrypt", async () => {
    const input = crypto.getRandomValues(new Uint8Array(512));
    const { ciphertext, encryptedKey } = await encryptDocument(
      input.buffer as ArrayBuffer,
      fixture.public_spki_pem,
    );

    mkdirSync(contractDir, { recursive: true });
    writeFileSync(
      join(contractDir, "browser_to_printer.vector.json"),
      JSON.stringify(
        {
          input_b64: bufferToBase64(input.buffer as ArrayBuffer),
          encrypted_key_b64: bufferToBase64(encryptedKey),
          ciphertext_b64: bufferToBase64(ciphertext),
        },
        null,
        2,
      ),
    );

    // Shape sanity: [12-byte IV || ciphertext || 16-byte GCM tag], RSA-4096 wrap.
    expect(ciphertext.byteLength).toBe(input.byteLength + 12 + 16);
    expect(encryptedKey.byteLength).toBe(512);
  });

  // Direction B: decrypt a Go-produced vector to catch any asymmetry the
  // production-only Direction A can't (e.g. a Go encoder that the browser
  // wouldn't accept).
  it("browser decrypts the Go-produced guard vector byte-for-byte", async () => {
    const v = JSON.parse(
      readFileSync(join(contractDir, "go_to_browser.vector.json"), "utf8"),
    ) as { input_b64: string; encrypted_key_b64: string; ciphertext_b64: string };

    const privKey = await crypto.subtle.importKey(
      "pkcs8",
      pemToDer(fixture.private_pkcs8_pem),
      { name: "RSA-OAEP", hash: "SHA-256" },
      false,
      ["decrypt"],
    );
    const aesRaw = await crypto.subtle.decrypt(
      { name: "RSA-OAEP" },
      privKey,
      b64ToBytes(v.encrypted_key_b64),
    );
    const aesKey = await crypto.subtle.importKey(
      "raw",
      aesRaw,
      { name: "AES-GCM" },
      false,
      ["decrypt"],
    );

    const blob = b64ToBytes(v.ciphertext_b64);
    const iv = blob.subarray(0, 12);
    const ct = blob.subarray(12);
    const plain = new Uint8Array(
      await crypto.subtle.decrypt({ name: "AES-GCM", iv }, aesKey, ct),
    );

    // Compare via base64 for a clean diff on mismatch.
    expect(bufferToBase64(plain.buffer as ArrayBuffer)).toBe(v.input_b64);
  });
});
