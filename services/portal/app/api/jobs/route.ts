import { NextRequest } from "next/server";
import { CLOUD_API_URL, forwardAuth, proxyJSON } from "@/lib/proxy";

export const dynamic = "force-dynamic";

// POST /api/jobs -> cloud POST /jobs. Auth optional: with no Authorization
// header the cloud server stores a guest job and returns a one-time
// guest_token. The request body carries encrypted_key opaquely; this proxy
// never parses or logs it (zero-knowledge invariant).
export async function POST(req: NextRequest) {
  const body = await req.text();
  const upstream = await fetch(`${CLOUD_API_URL}/jobs`, {
    method: "POST",
    headers: { "Content-Type": "application/json", ...forwardAuth(req) },
    body,
  });
  return proxyJSON(upstream);
}
