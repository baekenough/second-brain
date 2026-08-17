/**
 * Capture proxy — POST /api/notes
 *
 * Session-protected (web/src/proxy.ts catch-all — this path is NOT under
 * /api/v1/*, see that file's doc comment). Forwards the browser's note
 * content to the Go backend using the server-held API_KEY, mirroring
 * web/src/app/api/search/route.ts's pattern exactly.
 *
 * Request body: { title?: string; content: string }
 * Response: 202 { id: string; status: "pending" } (spec §6.1)
 */
import { type NextRequest, NextResponse } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

export async function POST(request: NextRequest): Promise<NextResponse> {
  const body = await request.text();

  try {
    const upstream = await fetch(`${BACKEND_URL}/api/v1/notes`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }),
      },
      body,
    });
    const data: unknown = await upstream.json().catch(() => ({}));
    return NextResponse.json(data, { status: upstream.status });
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : "Upstream request failed";
    return NextResponse.json({ error: message }, { status: 502 });
  }
}
