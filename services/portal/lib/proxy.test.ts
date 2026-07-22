// Unit tests for the thin API-proxy helpers (Testing Goal T6 / Part 4a).
// These enforce the "thin proxy" contract: forward auth only when present,
// relay upstream bodies opaquely (so encrypted_key passes through untouched),
// and rewrite the refresh cookie's path across the hop.
import { describe, it, expect } from "vitest";
import type { NextRequest } from "next/server";
import { forwardAuth, proxyJSON, proxyWithCookies, NODE_HEADER } from "./proxy";

// forwardAuth only reads req.headers.get(), so a Headers-bearing stub suffices.
function reqWith(headers: Record<string, string>): NextRequest {
  return { headers: new Headers(headers) } as unknown as NextRequest;
}

describe("forwardAuth — guest vs authenticated path selection", () => {
  it("forwards the Authorization header when present (authenticated flow)", () => {
    expect(forwardAuth(reqWith({ authorization: "Bearer abc" }))).toEqual({
      authorization: "Bearer abc",
    });
  });

  it("returns an empty object for the guest flow (no auth header)", () => {
    expect(forwardAuth(reqWith({}))).toEqual({});
  });

  it("copies only Authorization, never other headers", () => {
    const out = forwardAuth(reqWith({ cookie: "s=1", authorization: "Bearer z" }));
    expect(Object.keys(out)).toEqual(["authorization"]);
  });
});

describe("proxyJSON — opaque pass-through", () => {
  it("relays body byte-for-byte without inspecting it (encrypted_key opaque)", async () => {
    const body = JSON.stringify({ encrypted_key: "opaque++passes/through==", status: "queued" });
    const res = await proxyJSON(new Response(body, { status: 201 }));
    expect(res.status).toBe(201);
    expect(await res.text()).toBe(body);
    expect(res.headers.get("content-type")).toBe("application/json");
  });

  it("preserves upstream error status codes", async () => {
    const res = await proxyJSON(new Response(`{"error":"conflict"}`, { status: 409 }));
    expect(res.status).toBe(409);
    expect(await res.text()).toBe(`{"error":"conflict"}`);
  });

  it("forwards the cloud node header so the browser can show which node served it", async () => {
    const up = new Response("{}", {
      status: 200,
      headers: { [NODE_HEADER]: "cloud-server-2" },
    });
    const res = await proxyJSON(up);
    expect(res.headers.get(NODE_HEADER)).toBe("cloud-server-2");
  });

  it("never synthesises a node header when upstream sent none", async () => {
    const res = await proxyJSON(new Response("{}", { status: 200 }));
    expect(res.headers.get(NODE_HEADER)).toBeNull();
  });

  it("forwards no other upstream header (still a thin proxy)", async () => {
    const up = new Response("{}", {
      status: 200,
      headers: { "set-cookie": "leak=1", "x-internal-debug": "secret" },
    });
    const res = await proxyJSON(up);
    expect(res.headers.get("x-internal-debug")).toBeNull();
    expect(res.headers.getSetCookie()).toEqual([]);
  });
});

describe("proxyWithCookies — refresh cookie hop", () => {
  it("rewrites Path=/auth/refresh to Path=/ so the browser returns it to the proxy", async () => {
    const up = new Response("{}", { status: 200 });
    up.headers.append("set-cookie", "refresh=tok; Path=/auth/refresh; HttpOnly; Secure");
    const res = await proxyWithCookies(up);
    const sc = res.headers.getSetCookie().join("\n");
    expect(sc).toContain("Path=/");
    expect(sc).not.toContain("Path=/auth/refresh");
    expect(sc).toContain("HttpOnly"); // other attributes preserved
  });

  it("forwards every Set-Cookie header", async () => {
    const up = new Response("{}", { status: 200 });
    up.headers.append("set-cookie", "a=1; Path=/auth/refresh");
    up.headers.append("set-cookie", "b=2; Path=/auth/refresh");
    const res = await proxyWithCookies(up);
    expect(res.headers.getSetCookie().length).toBe(2);
  });
});
