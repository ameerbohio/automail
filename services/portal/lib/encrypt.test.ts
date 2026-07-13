// Unit tests for the browser encryptor (Testing Goal T6 / Part 4a). Focus on
// the wire-format mechanics — IV prepend, GCM tag, RSA-OAEP key wrap, per-call
// randomness — and the chunked base64 encoder. The cross-language byte-for-byte
// contract with the printer lives separately in crypto-contract.contract.test.ts.
import { describe, it, expect, beforeAll } from "vitest";
import { encryptDocument, bufferToBase64, type EncryptedJob } from "./encrypt";

let publicPem: string;
let privateKey: CryptoKey;

beforeAll(async () => {
  // A throwaway RSA-2048 keypair (fast); the code path is key-size agnostic.
  const kp = await crypto.subtle.generateKey(
    { name: "RSA-OAEP", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" },
    true,
    ["encrypt", "decrypt"],
  );
  privateKey = kp.privateKey;
  const spki = await crypto.subtle.exportKey("spki", kp.publicKey);
  const b64 = Buffer.from(new Uint8Array(spki)).toString("base64");
  publicPem = `-----BEGIN PUBLIC KEY-----\n${b64}\n-----END PUBLIC KEY-----`;
});

async function unwrapAndDecrypt(enc: EncryptedJob): Promise<Uint8Array> {
  const aesRaw = await crypto.subtle.decrypt({ name: "RSA-OAEP" }, privateKey, enc.encryptedKey);
  const aesKey = await crypto.subtle.importKey("raw", aesRaw, { name: "AES-GCM" }, false, ["decrypt"]);
  const blob = new Uint8Array(enc.ciphertext);
  const iv = blob.subarray(0, 12);
  const ct = blob.subarray(12);
  return new Uint8Array(await crypto.subtle.decrypt({ name: "AES-GCM", iv }, aesKey, ct));
}

describe("encryptDocument", () => {
  it("prepends a 12-byte IV and appends the 16-byte GCM tag", async () => {
    const input = new Uint8Array(100).fill(7);
    const enc = await encryptDocument(input.buffer, publicPem);
    expect(enc.ciphertext.byteLength).toBe(100 + 12 + 16);
    expect(enc.encryptedKey.byteLength).toBe(256); // RSA-2048 wrap
  });

  it("round-trips: the wrapped key + prepended IV decrypt back to the input", async () => {
    const input = crypto.getRandomValues(new Uint8Array(512));
    const enc = await encryptDocument(input.buffer, publicPem);
    const out = await unwrapAndDecrypt(enc);
    expect(Buffer.from(out).equals(Buffer.from(input))).toBe(true);
  });

  it("uses a fresh IV and content key on every call (no reuse)", async () => {
    const input = new Uint8Array([1, 2, 3, 4]).buffer;
    const a = await encryptDocument(input, publicPem);
    const b = await encryptDocument(input, publicPem);
    expect(Buffer.from(a.ciphertext).equals(Buffer.from(b.ciphertext))).toBe(false);
    expect(Buffer.from(a.encryptedKey).equals(Buffer.from(b.encryptedKey))).toBe(false);
  });

  it("handles an empty document (IV + tag only)", async () => {
    const enc = await encryptDocument(new ArrayBuffer(0), publicPem);
    expect(enc.ciphertext.byteLength).toBe(12 + 16);
    expect((await unwrapAndDecrypt(enc)).byteLength).toBe(0);
  });

  it("rejects a malformed public-key PEM", async () => {
    await expect(encryptDocument(new Uint8Array([1]).buffer, "not a pem")).rejects.toBeDefined();
  });
});

describe("bufferToBase64", () => {
  it("produces standard base64 that round-trips", () => {
    const bytes = new Uint8Array([0, 1, 2, 250, 251, 255]);
    const b64 = bufferToBase64(bytes.buffer);
    expect(b64).toBe(Buffer.from(bytes).toString("base64"));
    expect(Buffer.from(b64, "base64").equals(Buffer.from(bytes))).toBe(true);
  });

  it("chunks correctly for buffers larger than the 0x8000 chunk size", () => {
    const big = new Uint8Array(70000); // > 32768, exercises the chunk loop
    for (let i = 0; i < big.length; i++) big[i] = i % 256;
    expect(bufferToBase64(big.buffer)).toBe(Buffer.from(big).toString("base64"));
  });

  it("returns an empty string for an empty buffer", () => {
    expect(bufferToBase64(new ArrayBuffer(0))).toBe("");
  });
});
