/**
 * Documents proxy — GET /api/v1/documents/recent
 *
 * Proxies the mobile app's dashboard count fetch to the backend.
 * Forwards query params (kind, limit, and any others) verbatim.
 * Authentication: forwards the mobile app's Bearer API_KEY header as-is.
 *   (This route is excluded from OAuth middleware — see proxy.ts / middleware.ts)
 *
 * Query params (forwarded verbatim):
 *   kind=sms|call|recording|gmail|calendar  (optional)
 *   limit=<number>                           (optional)
 *
 * Response:
 *   200 { documents: [...], total: number }
 *   502 { error: string } on upstream fetch failure
 */
import { type NextRequest, NextResponse } from "next/server";

function backendUrl(path: string): string {
  const base =
    process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
  return `${base.replace(/\/$/, "")}${path}`;
}

export async function GET(req: NextRequest): Promise<NextResponse> {
  const authorization = req.headers.get("authorization") ?? "";

  // Forward the incoming query string verbatim (kind, limit, etc.)
  const search = req.nextUrl.search; // includes leading "?" or empty string

  let upstream: Response;
  try {
    upstream = await fetch(backendUrl(`/api/v1/documents/recent${search}`), {
      method: "GET",
      headers: {
        Authorization: authorization,
      },
    });
  } catch (err) {
    const message = err instanceof Error ? err.message : "upstream error";
    return NextResponse.json({ error: message }, { status: 502 });
  }

  // Forward upstream body and status code verbatim
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": "application/json" },
  });
}
