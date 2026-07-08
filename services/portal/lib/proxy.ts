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

// proxyJSON relays an upstream JSON response verbatim (status + body). The
// portal adds nothing and inspects nothing: a thin pass-through so the browser
// sees the cloud server's own contract and error codes (the roadmap Phase 7
// requirement that Next.js API routes be "thin proxies"). In particular it
// never parses or logs the body -- encrypted_key must pass through opaque.
export async function proxyJSON(upstream: Response): Promise<Response> {
  const body = await upstream.text();
  return new Response(body, {
    status: upstream.status,
    headers: { "Content-Type": "application/json" },
  });
}
