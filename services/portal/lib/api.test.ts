// Unit tests for the client-side API layer (Testing Goal T6 / Part 4a).
//
// lib/api.ts had no tests: it was added in Phase 7 and grew the served_by
// plumbing later, so the portal's whole request contract -- which endpoint each
// call hits, which ones carry the Bearer token, and how a cloud-server error is
// surfaced to the user -- was only ever exercised end to end through Playwright.
// These cover it directly, including uploadBlob, the one call that leaves the
// portal's origin.
import { describe, it, expect, vi, afterEach } from "vitest";
import {
  searchRecipients,
  getRecipientPublicKey,
  requestUploadURL,
  uploadBlob,
  createJob,
} from "./api";

type FetchCall = { url: string; init: RequestInit | undefined };

/** Stubs global fetch with a fixed response and records what was requested. */
function stubFetch(res: Response): FetchCall[] {
  const calls: FetchCall[] = [];
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ url: String(input), init });
    return Promise.resolve(res);
  });
  return calls;
}

function json(body: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
    ...init,
  });
}

afterEach(() => vi.unstubAllGlobals());

describe("searchRecipients", () => {
  it("URL-encodes the query so a name with spaces or & cannot break the URL", async () => {
    const calls = stubFetch(json([]));
    await searchRecipients("Testmann & Co");
    expect(calls[0].url).toBe("/api/recipients?q=Testmann%20%26%20Co");
  });

  it("returns the recipient list", async () => {
    stubFetch(json([{ recipient_id: "r1", display_name: "R. Testmann", building_address: "88 Waverley" }]));
    await expect(searchRecipients("Test")).resolves.toHaveLength(1);
  });
});

describe("getRecipientPublicKey", () => {
  it("unwraps public_key_pem from the response envelope", async () => {
    stubFetch(json({ recipient_id: "r1", public_key_pem: "-----BEGIN PUBLIC KEY-----" }));
    await expect(getRecipientPublicKey("r1")).resolves.toBe("-----BEGIN PUBLIC KEY-----");
  });

  it("encodes the id into the path", async () => {
    const calls = stubFetch(json({ public_key_pem: "x" }));
    await getRecipientPublicKey("a/b");
    expect(calls[0].url).toBe("/api/recipients/a%2Fb/public-key");
  });
});

describe("error surfacing", () => {
  // jsonOrThrow deliberately re-raises the cloud server's own {error, code}
  // message so a failure reads as "recipient not found or slot unassigned"
  // rather than an opaque status code.
  it("raises the cloud server's error message, not the status code", async () => {
    stubFetch(json({ error: "recipient not found or slot unassigned", code: "RECIPIENT_NOT_FOUND" }, { status: 400 }));
    await expect(searchRecipients("nobody")).rejects.toThrow("recipient not found or slot unassigned");
  });

  it("falls back to the status code when the error body is not JSON", async () => {
    vi.stubGlobal("fetch", () =>
      Promise.resolve(new Response("<html>502 Bad Gateway</html>", { status: 502 })),
    );
    await expect(searchRecipients("x")).rejects.toThrow("request failed (502)");
  });
});

describe("requestUploadURL", () => {
  it("posts the recipient and filename", async () => {
    const calls = stubFetch(json({ upload_url: "https://blob.example/put", blob_ref: "b/1", expires_in: 900 }));
    await requestUploadURL("r1", "letter.pdf.enc");
    expect(calls[0].url).toBe("/api/jobs/upload-url");
    expect(calls[0].init?.method).toBe("POST");
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({
      recipient_id: "r1",
      filename: "letter.pdf.enc",
    });
  });

  it("carries the serving node through as served_by", async () => {
    stubFetch(
      json({ upload_url: "u", blob_ref: "b", expires_in: 900 }, { headers: { "x-automail-node": "cloud-server-1" } }),
    );
    await expect(requestUploadURL("r1", "f")).resolves.toMatchObject({ served_by: "cloud-server-1" });
  });

  it("leaves served_by undefined when the header is absent", async () => {
    stubFetch(json({ upload_url: "u", blob_ref: "b", expires_in: 900 }));
    await expect(requestUploadURL("r1", "f")).resolves.toMatchObject({ served_by: undefined });
  });
});

describe("uploadBlob", () => {
  // The ONE request that leaves the portal's origin: the browser PUTs the
  // ciphertext straight to object storage so the cloud server never holds the
  // blob (plans/08). Everything else goes through the same-origin /api proxy.
  it("PUTs the ciphertext as an opaque octet-stream to the presigned URL", async () => {
    const calls = stubFetch(new Response("", { status: 200 }));
    const ciphertext = new Uint8Array([1, 2, 3]).buffer;
    await uploadBlob("https://blob.automail.local/bucket/key?sig=abc", ciphertext);
    expect(calls[0].url).toBe("https://blob.automail.local/bucket/key?sig=abc");
    expect(calls[0].init?.method).toBe("PUT");
    expect((calls[0].init?.headers as Record<string, string>)["Content-Type"]).toBe(
      "application/octet-stream",
    );
    expect(calls[0].init?.body).toBe(ciphertext);
  });

  it("throws with the status when object storage rejects the upload", async () => {
    stubFetch(new Response("", { status: 403 }));
    await expect(uploadBlob("https://blob/x", new ArrayBuffer(1))).rejects.toThrow(
      "blob upload failed (403)",
    );
  });
});

describe("createJob", () => {
  const input = { encrypted_key: "opaque++base64/==", blob_ref: "b/1", recipient_id: "r1", page_count: 2 };

  it("sends no Authorization header for a guest submission", async () => {
    const calls = stubFetch(json({ job_id: "j1", status: "submitted", guest_token: "gt_x" }));
    await createJob(input);
    const headers = calls[0].init?.headers as Record<string, string>;
    expect(headers.Authorization).toBeUndefined();
  });

  it("sends a Bearer token when the sender is authenticated", async () => {
    const calls = stubFetch(json({ job_id: "j1", status: "submitted" }));
    await createJob(input, "jwt-access-token");
    const headers = calls[0].init?.headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer jwt-access-token");
  });

  it("passes encrypted_key through opaquely (zero-knowledge: never parsed)", async () => {
    const calls = stubFetch(json({ job_id: "j1", status: "submitted" }));
    await createJob(input, null);
    expect(JSON.parse(String(calls[0].init?.body)).encrypted_key).toBe("opaque++base64/==");
  });

  it("carries the serving node through as served_by", async () => {
    stubFetch(json({ job_id: "j1", status: "submitted" }, { headers: { "x-automail-node": "cloud-server-2" } }));
    await expect(createJob(input)).resolves.toMatchObject({ served_by: "cloud-server-2" });
  });
});
