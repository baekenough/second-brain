/**
 * Ingest proxy — POST /api/v1/ingest/messages
 *
 * Pass-through to the backend ingest endpoint.
 * Authentication: forwards the mobile app's Bearer API_KEY header as-is.
 *   (This route is excluded from OAuth middleware — see middleware.ts)
 *
 * Request body (JSON, forwarded verbatim):
 *   { sms: [{address, body, date_ms, type, contact_name}],
 *     calls: [{number, date_ms, duration_sec, type, contact_name}] }
 *
 * Response:
 *   201 { accepted: number, skipped: number, errors: string[] }
 */
import { type NextRequest, NextResponse } from "next/server";

function backendUrl(path: string): string {
  const base =
    process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
  return `${base.replace(/\/$/, "")}${path}`;
}

export async function POST(req: NextRequest): Promise<NextResponse> {
  const authorization = req.headers.get("authorization") ?? "";
  const contentType = req.headers.get("content-type") ?? "application/json";

  // Buffer the body so we can forward it without streaming (avoids duplex issues)
  const body = await req.arrayBuffer();

  let upstream: Response;
  try {
    upstream = await fetch(backendUrl("/api/v1/ingest/messages"), {
      method: "POST",
      headers: {
        Authorization: authorization,
        "Content-Type": contentType,
      },
      body,
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
