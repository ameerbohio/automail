import { NextRequest } from "next/server";
import { CLOUD_API_URL, proxyWithCookies } from "@/lib/proxy";

export const dynamic = "force-dynamic";

// POST /api/auth/login -> cloud POST /auth/login. Relays the refresh cookie.
export async function POST(req: NextRequest) {
  const body = await req.text();
  const upstream = await fetch(`${CLOUD_API_URL}/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body,
  });
  return proxyWithCookies(upstream);
}
