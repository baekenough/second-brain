/**
 * Ask conversation detail proxy — GET /api/ask/conversations/{id}
 *
 * Session-protected (web/src/proxy.ts catch-all). Forwards to the Go
 * backend's GET /api/v1/ask/conversations/{id} using the server-held
 * API_KEY, mirroring web/src/app/api/documents/[id]/route.ts's pattern.
 * Returns every turn of one conversation ordered turn_index ASC, used to
 * restore the /ask screen after a page refresh.
 */
import { type NextRequest, NextResponse } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

export async function GET(
  _request: NextRequest,
  { params }: { params: Promise<{ id: string }> },
): Promise<NextResponse> {
  const { id } = await params;

  const upstreamUrl = `${BACKEND_URL}/api/v1/ask/conversations/${encodeURIComponent(id)}`;

  try {
    const upstream = await fetch(upstreamUrl, {
      headers: {
        "Content-Type": "application/json",
        ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }),
      },
      cache: "no-store",
    });
    const data: unknown = await upstream.json().catch(() => ({}));
    return NextResponse.json(data, { status: upstream.status });
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : "Upstream request failed";
    return NextResponse.json({ error: message }, { status: 502 });
  }
}
