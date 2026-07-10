import { NextRequest } from "next/server";
import { CLOUD_API_URL, forwardAuth } from "@/lib/proxy";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

// GET /api/jobs/:id/stream?token=... -> cloud GET /jobs/:id/stream?token=...
// SSE pass-through. The browser's EventSource cannot set an Authorization
// header, so the guest token rides in the query string (plans/09-api-
// contracts.md). We stream the upstream body straight through and forward the
// client's abort signal so the cloud server sees the disconnect and unsubs
// from Redis (its r.Context().Done() path in StreamJob).
export async function GET(
  req: NextRequest,
  { params }: { params: { id: string } },
) {
  // Two auth modes, both required because EventSource cannot set headers:
  //   - guest:         ?token=<guest_token>  -> forwarded as ?token=
  //   - authenticated: ?access=<jwt>         -> sent up as Authorization Bearer
  // The access token is short-lived and passed only at connect time; the cloud
  // server does the JWT ownership check (StreamJob/authorizeStream).
  const token = req.nextUrl.searchParams.get("token");
  const access = req.nextUrl.searchParams.get("access");
  const headers: Record<string, string> = {
    Accept: "text/event-stream",
    ...forwardAuth(req),
  };
  if (access) headers.Authorization = `Bearer ${access}`;
  const qs = token ? `?token=${encodeURIComponent(token)}` : "";
  const upstream = await fetch(
    `${CLOUD_API_URL}/jobs/${encodeURIComponent(params.id)}/stream${qs}`,
    {
      headers,
      signal: req.signal,
    },
  );

  // On an auth/lookup failure the cloud server responds with a normal JSON
  // error (not an event stream) -- relay it as JSON so the client sees the
  // reason instead of an opaque stream error.
  if (!upstream.ok || !upstream.body) {
    const errBody = await upstream.text().catch(() => "");
    return new Response(errBody || '{"error":"stream unavailable"}', {
      status: upstream.ok ? 502 : upstream.status,
      headers: { "Content-Type": "application/json" },
    });
  }

  return new Response(upstream.body, {
    status: 200,
    headers: {
      "Content-Type": "text/event-stream",
      "Cache-Control": "no-cache, no-transform",
      Connection: "keep-alive",
    },
  });
}
