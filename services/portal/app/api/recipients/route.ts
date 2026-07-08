import { NextRequest } from "next/server";
import { CLOUD_API_URL, forwardAuth, proxyJSON } from "@/lib/proxy";

export const dynamic = "force-dynamic";

// GET /api/recipients?q=... -> cloud GET /recipients?q=... (no auth, guest).
export async function GET(req: NextRequest) {
  const q = req.nextUrl.searchParams.get("q") ?? "";
  const upstream = await fetch(
    `${CLOUD_API_URL}/recipients?q=${encodeURIComponent(q)}`,
    { headers: forwardAuth(req) },
  );
  return proxyJSON(upstream);
}
