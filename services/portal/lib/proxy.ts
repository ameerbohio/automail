import { NextRequest } from "next/server";

// Base URL of the cloud server, reachable server-side on the Docker network.
// Never exposed to the browser -- browser calls all hit same-origin /api
// routes, which relay here. Defaults to the compose service name.
export const CLOUD_API_URL =
  process.env.CLOUD_API_URL ?? "http://cloud-server:8080";

// forwardAuth copies the Authorization header through the proxy when present.
// The guest flow (Phase 7) sends none; Phase 8's authenticated flow will.
// Returns a plain record so it spreads cleanly into a headers object.
export function forwardAuth(req: NextRequest): Record<string, string> {
  const auth = req.headers.get("authorization");
  return auth ? { authorization: auth } : {};
}

// Names the cloud node that served the upstream response (cloud
// middleware.go NodeHeader). Forwarded through the proxy so the browser can
// show which of the N stateless nodes handled a submission.
export const NODE_HEADER = "x-automail-node";

// proxyJSON relays an upstream JSON response verbatim (status + body). The
// portal adds nothing and inspects nothing: a thin pass-through so the browser
// sees the cloud server's own contract and error codes (the roadmap Phase 7
// requirement that Next.js API routes be "thin proxies"). In particular it
// never parses or logs the body -- encrypted_key must pass through opaque.
export async function proxyJSON(upstream: Response): Promise<Response> {
  const body = await upstream.text();
  const headers = new Headers({ "Content-Type": "application/json" });
  // The one upstream header that is copied through. Still a thin proxy: it is
  // relayed as-is, never synthesised, and absent if upstream didn't send it.
  const node = upstream.headers.get(NODE_HEADER);
  if (node) headers.set(NODE_HEADER, node);
  return new Response(body, { status: upstream.status, headers });
}

// The cloud server scopes its refresh cookie to Path=/auth/refresh, but the
// browser only ever talks to the portal's proxy routes. Rewrite the path to
// "/" so the browser returns the cookie to /api/auth/refresh AND so the
// account-page middleware can see it. Covers both the set (login/register/
// refresh) and the clear (logout) cookie, since both use the same path string.
function rewriteCookiePath(cookie: string): string {
  return cookie.replace(/Path=\/auth\/refresh/gi, "Path=/");
}

// proxyWithCookies relays an upstream auth response verbatim (status + body)
// and forwards its Set-Cookie header(s) with the path rewritten. Used by the
// /api/auth/* proxies so the HttpOnly refresh cookie survives the hop.
export async function proxyWithCookies(upstream: Response): Promise<Response> {
  const body = await upstream.text();
  const headers = new Headers({
    "Content-Type": upstream.headers.get("content-type") ?? "application/json",
  });
  for (const cookie of upstream.headers.getSetCookie()) {
    headers.append("set-cookie", rewriteCookiePath(cookie));
  }
  return new Response(body || null, { status: upstream.status, headers });
}
