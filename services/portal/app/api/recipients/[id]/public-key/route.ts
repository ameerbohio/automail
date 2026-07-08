import { NextRequest } from "next/server";
import { CLOUD_API_URL, forwardAuth, proxyJSON } from "@/lib/proxy";

export const dynamic = "force-dynamic";

// GET /api/recipients/:id/public-key -> cloud GET /recipients/:id/public-key.
// Returns the recipient printer's RSA public key; the sender never sees the
// mailbox or slot ID (plans/09-api-contracts.md).
export async function GET(
  req: NextRequest,
  { params }: { params: { id: string } },
) {
  const upstream = await fetch(
    `${CLOUD_API_URL}/recipients/${encodeURIComponent(params.id)}/public-key`,
    { headers: forwardAuth(req) },
  );
  return proxyJSON(upstream);
}
