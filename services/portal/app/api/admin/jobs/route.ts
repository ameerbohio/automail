import { NextRequest } from "next/server";
import { CLOUD_API_URL, forwardAuth, proxyJSON } from "@/lib/proxy";

export const dynamic = "force-dynamic";

// GET /api/admin/jobs?status=&page=&per_page= -> cloud GET /admin/jobs.
// The status filter + pagination query string passes straight through;
// admin-role JWT is enforced upstream (requireAdmin).
export async function GET(req: NextRequest) {
  const upstream = await fetch(`${CLOUD_API_URL}/admin/jobs${req.nextUrl.search}`, {
    headers: forwardAuth(req),
  });
  return proxyJSON(upstream);
}
