/**
 * Next.js Edge Middleware — Cloudflare Access authentication gate.
 *
 * This app runs behind Cloudflare Access. Access itself handles the login
 * UI (identity provider selection, MFA, etc.) and, once a visitor is
 * authenticated, attaches a signed JWT to every proxied request — either as
 * the `Cf-Access-Jwt-Assertion` request header or the `CF_Authorization`
 * cookie. This middleware's only job is to verify that JWT is genuine
 * before letting the request reach any route.
 *
 * Why verify the JWT instead of trusting the `Cf-Access-Authenticated-User-Email`
 * header: docker-compose.local.yml publishes the web container's port 3000
 * directly on the host, so the container is reachable from the LAN/Tailscale
 * without going through the Cloudflare tunnel. Any client that can reach
 * that port can set arbitrary request headers, including
 * `Cf-Access-Authenticated-User-Email`. Trusting that header (or any other
 * Access header) without checking the JWT signature would let anyone on the
 * LAN impersonate an authenticated user. Verifying the JWT's signature
 * against Cloudflare's published JWKS, issuer, audience, and expiry closes
 * that hole — a request can only pass if it carries a token Cloudflare
 * actually signed for this Access application.
 *
 * Configuration (see .env.example):
 *   CF_ACCESS_TEAM_DOMAIN — e.g. "your-team.cloudflareaccess.com"
 *   CF_ACCESS_AUD         — Access application AUD tag
 *
 * Fail-closed: if either env var is missing, ALL requests are rejected
 * with a 503. There is no "pass through when unconfigured" mode — an
 * unconfigured gate must never behave like an open gate.
 *
 * Note: next-auth (Auth.js) wiring — src/auth.ts, src/app/login/,
 * src/app/api/auth/*, and the next-auth dependency — is no longer
 * referenced by this middleware and is now dead code. It is intentionally
 * left in place for this change to keep the diff reviewable; removing it
 * is follow-up cleanup.
 */
import { jwtVerify, createRemoteJWKSet, type JWTPayload } from "jose";
import { NextResponse } from "next/server";
import type { NextRequest } from "next/server";

const TEAM_DOMAIN = process.env.CF_ACCESS_TEAM_DOMAIN;
const AUD = process.env.CF_ACCESS_AUD;

// createRemoteJWKSet caches the fetched JWKS internally and only
// re-fetches on a verification failure that looks like a key rotation,
// so we do not hit Cloudflare's certs endpoint on every request.
const JWKS =
  TEAM_DOMAIN && AUD
    ? createRemoteJWKSet(new URL(`https://${TEAM_DOMAIN}/cdn-cgi/access/certs`))
    : null;

function isApiRoute(pathname: string): boolean {
  return pathname.startsWith("/api/");
}

function unauthorized(req: NextRequest, message: string): NextResponse {
  if (isApiRoute(req.nextUrl.pathname)) {
    return NextResponse.json({ error: message }, { status: 401 });
  }
  // Non-API page request: return 401 directly rather than redirecting.
  // Cloudflare Access sits in front of this app and already presents its
  // own login UI for unauthenticated browser requests; issuing our own
  // redirect here (e.g. to a local /login page that no longer performs
  // any auth) would either 404 or create a redirect loop back through
  // Access.
  return NextResponse.json({ error: message }, { status: 401 });
}

function extractToken(req: NextRequest): string | null {
  const headerToken = req.headers.get("Cf-Access-Jwt-Assertion");
  if (headerToken) return headerToken;

  const cookieToken = req.cookies.get("CF_Authorization")?.value;
  if (cookieToken) return cookieToken;

  return null;
}

export default async function proxy(req: NextRequest): Promise<NextResponse> {
  // Fail-closed: without both env vars we cannot verify anything, so we
  // must not let any request through.
  if (!TEAM_DOMAIN || !AUD || !JWKS) {
    return NextResponse.json(
      {
        error:
          "Cloudflare Access is not configured (missing CF_ACCESS_TEAM_DOMAIN or CF_ACCESS_AUD). Denying all requests.",
      },
      { status: 503 },
    );
  }

  const token = extractToken(req);
  if (!token) {
    return unauthorized(req, "Missing Cloudflare Access token");
  }

  let payload: JWTPayload;
  try {
    const result = await jwtVerify(token, JWKS, {
      issuer: `https://${TEAM_DOMAIN}`,
      audience: AUD,
    });
    payload = result.payload;
  } catch {
    return unauthorized(req, "Invalid or expired Cloudflare Access token");
  }

  const response = NextResponse.next();
  const email = typeof payload.email === "string" ? payload.email : undefined;
  if (email) {
    response.headers.set("X-Access-User-Email", email);
  }
  return response;
}

/**
 * Matcher: run middleware on all routes EXCEPT Next.js internals and
 * static files. Every application route — pages and API routes alike,
 * including /api/v1/* and /api/auth/* — must pass Cloudflare Access
 * verification. The mobile app no longer talks to this service (it now
 * connects directly to the Go backend through its own tunnel), so there
 * is no remaining reason to exempt /api/v1/* from this gate; keeping that
 * exemption would leave an unauthenticated bypass.
 */
export const config = {
  matcher: ["/((?!_next/static|_next/image|favicon\\.ico|robots\\.txt).*)"],
};
