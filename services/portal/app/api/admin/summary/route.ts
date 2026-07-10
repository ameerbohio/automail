import { NextRequest } from "next/server";
import { CLOUD_API_URL, forwardAuth, proxyJSON } from "@/lib/proxy";

export const dynamic = "force-dynamic";

// GET /api/admin/summary -> cloud GET /admin/summary. Admin-role JWT enforced
// upstream (requireAdmin); this thin proxy just forwards the Bearer token.
export async function GET(req: NextRequest) {
  const upstream = await fetch(`${CLOUD_API_URL}/admin/summary`, {
    headers: forwardAuth(req),
  });
  return proxyJSON(upstream);
}
