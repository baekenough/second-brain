/**
 * Capture proxy — POST /api/notes/{id}/retry-enrichment
 * Only valid for notes in the terminal "failed" state; backend returns
 * 409 Conflict otherwise (spec §6.3, §9.2) — forwarded verbatim.
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

  try {
    const upstream = await fetch(
      `${BACKEND_URL}/api/v1/notes/${encodeURIComponent(id)}/retry-enrichment`,
      {
        method: "POST",
        headers: { ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }) },
      },
    );
    const data: unknown = await upstream.json().catch(() => ({}));
    return NextResponse.json(data, { status: upstream.status });
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : "Upstream request failed";
    return NextResponse.json({ error: message }, { status: 502 });
  }
}
