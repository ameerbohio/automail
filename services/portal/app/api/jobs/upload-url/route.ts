import { NextRequest } from "next/server";
import { CLOUD_API_URL, forwardAuth, proxyJSON } from "@/lib/proxy";

export const dynamic = "force-dynamic";

// POST /api/jobs/upload-url -> cloud POST /jobs/upload-url. Auth optional.
export async function POST(req: NextRequest) {
  const body = await req.text();
  const upstream = await fetch(`${CLOUD_API_URL}/jobs/upload-url`, {
    method: "POST",
    headers: { "Content-Type": "application/json", ...forwardAuth(req) },
    body,
  });
  return proxyJSON(upstream);
}
