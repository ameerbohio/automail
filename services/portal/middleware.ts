import { NextRequest, NextResponse } from "next/server";

// Account-only pages. The guest flow (/, /track) is deliberately NOT gated --
// sending mail never requires an account (plans/10 Phase 8: "guest flow still
// works unauthenticated").
export function middleware(req: NextRequest) {
  // Lightweight gate: presence of the refresh cookie (rewritten to Path=/ by
  // the auth proxies). This only avoids flashing an account page to a guest
  // before the client redirects -- the real enforcement is the cloud API
  // returning 401, which the pages also handle.
  if (!req.cookies.has("refresh_token")) {
    const url = req.nextUrl.clone();
    url.pathname = "/login";
    url.searchParams.set("next", req.nextUrl.pathname);
    return NextResponse.redirect(url);
  }
  return NextResponse.next();
}

export const config = {
  // Only account pages. Guest routes and /api/* are excluded.
  matcher: ["/history/:path*", "/jobs/:path*"],
};
