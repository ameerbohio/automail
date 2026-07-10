import { NextRequest } from "next/server";
import { CLOUD_API_URL, proxyWithCookies } from "@/lib/proxy";

export const dynamic = "force-dynamic";

// POST /api/auth/refresh -> cloud POST /auth/refresh. Forwards the browser's
// refresh cookie up and relays the rotated cookie back down. The refresh token
// is single-use, so the cloud rotates it on every call.
export async function POST(req: NextRequest) {
  const cookie = req.headers.get("cookie");
  const upstream = await fetch(`${CLOUD_API_URL}/auth/refresh`, {
    method: "POST",
    headers: cookie ? { cookie } : {},
  });
  return proxyWithCookies(upstream);
}
