/**
 * Ask conversations proxy — GET /api/ask/conversations?limit=N
 *
 * Session-protected (web/src/proxy.ts catch-all — this path is NOT under
 * /api/v1/*, mirroring web/src/app/api/documents/route.ts's GET pattern).
 * Forwards to the Go backend's GET /api/v1/ask/conversations using the
 * server-held API_KEY, returning the latest turn of each conversation
 * (most recently active first) for the /ask screen's recent-conversations
 * list.
 */
import { type NextRequest, NextResponse } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

export async function GET(request: NextRequest): Promise<NextResponse> {
  const limit = request.nextUrl.searchParams.get("limit");

  const url = new URL(`${BACKEND_URL}/api/v1/ask/conversations`);
  if (limit) url.searchParams.set("limit", limit);

  try {
    const upstream = await fetch(url.toString(), {
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
