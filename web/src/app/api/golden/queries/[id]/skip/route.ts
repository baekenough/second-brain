/**
 * Golden set proxy — POST /api/golden/queries/{id}/skip
 *   → POST /api/v1/golden/queries/{id}/skip
 *
 * No request body; the id is a query identifier, not personal content, but
 * is kept out of log lines anyway for consistency with the other golden
 * routes.
 */
import { type NextRequest, NextResponse } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

export async function POST(
  _request: NextRequest,
  { params }: { params: Promise<{ id: string }> },
): Promise<NextResponse> {
  const { id } = await params;
  if (!id) {
    return NextResponse.json({ error: "invalid query id" }, { status: 400 });
  }

  try {
    const upstream = await fetch(
      `${BACKEND_URL}/api/v1/golden/queries/${encodeURIComponent(id)}/skip`,
      {
        method: "POST",
        headers: { ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }) },
      },
    );
    const text = await upstream.text();
    return new NextResponse(text.length > 0 ? text : null, {
      status: upstream.status,
      headers: { "Content-Type": "application/json" },
    });
  } catch (error: unknown) {
    console.error(
      "[api/golden/queries/skip] upstream request failed:",
      error instanceof Error ? error.name : "unknown",
    );
    return NextResponse.json({ error: "upstream request failed" }, { status: 502 });
  }
}
