import { NextRequest } from "next/server";
import { CLOUD_API_URL, proxyWithCookies } from "@/lib/proxy";

export const dynamic = "force-dynamic";

// POST /api/auth/logout -> cloud POST /auth/logout. Needs the Bearer token
// (requireAuth) to revoke the session and the cookie to clear it; relays the
// cleared cookie back.
export async function POST(req: NextRequest) {
  const headers: Record<string, string> = {};
  const cookie = req.headers.get("cookie");
  const auth = req.headers.get("authorization");
  if (cookie) headers.cookie = cookie;
  if (auth) headers.authorization = auth;
  const upstream = await fetch(`${CLOUD_API_URL}/auth/logout`, {
    method: "POST",
    headers,
  });
  return proxyWithCookies(upstream);
}
