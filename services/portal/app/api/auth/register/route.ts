import { NextRequest } from "next/server";
import { CLOUD_API_URL, proxyWithCookies } from "@/lib/proxy";

export const dynamic = "force-dynamic";

// POST /api/auth/register -> cloud POST /auth/register. Open self-service
// signup; the cloud auto-logs-in, so the 201 carries an access token and sets
// the refresh cookie, which we relay.
export async function POST(req: NextRequest) {
  const body = await req.text();
  const upstream = await fetch(`${CLOUD_API_URL}/auth/register`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body,
  });
  return proxyWithCookies(upstream);
}
