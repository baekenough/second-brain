/**
 * Next.js Edge Middleware — authentication gate.
 *
 * Protected routes (require GitHub OAuth session):
 *   - All page routes: /, /dashboard, /governance, /documents/*
 *   - Internal API proxy routes: /api/search, /api/documents/*, /api/stats/*, /api/sources
 *
 * Excluded from auth (pass-through):
 *   - /api/auth/*              — Auth.js sign-in / callback / sign-out
 *   - /api/v1/*                — All mobile proxy routes (Bearer API_KEY, not OAuth)
 *   - /_next/static, /_next/image — Next.js static assets
 *   - /favicon.ico, /robots.txt
 */
import { auth } from "@/auth";
import { NextResponse } from "next/server";
import type { NextRequest } from "next/server";

export default auth((req: NextRequest & { auth: unknown }) => {
  // auth() injects req.auth = session | null
  const session = (req as { auth: unknown }).auth;

  if (!session) {
    // Redirect to custom login page, preserving the original URL as callbackUrl
    const signInUrl = new URL("/login", req.url);
    signInUrl.searchParams.set("callbackUrl", req.url);
    return NextResponse.redirect(signInUrl);
  }

  return NextResponse.next();
});

/**
 * Matcher: run middleware on all routes EXCEPT:
 *   - Next.js internals (_next/*)
 *   - Static files (favicon, robots, etc.)
 *   - Auth.js routes (api/auth/*)
 *   - api/v1/* (all mobile proxy routes — Bearer API_KEY, not OAuth)
 *     Covers: /api/v1/ingest/messages, /api/v1/ingest/recording,
 *             /api/v1/ingest/file, /api/v1/documents/recent, and any
 *             future mobile-facing endpoints under /api/v1.
 */
export const config = {
  matcher: ["/((?!_next/static|_next/image|favicon\\.ico|robots\\.txt|api/auth|api/v1|login).*)"],
};
