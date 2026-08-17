/**
 * Capture proxy — DELETE /api/notes/{id}
 * Cascades to derived insight documents on the backend (spec §6.5).
 */
import { type NextRequest, NextResponse } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

export async function DELETE(
  _request: NextRequest,
  { params }: { params: Promise<{ id: string }> },
): Promise<NextResponse> {
  const { id } = await params;

  try {
    const upstream = await fetch(`${BACKEND_URL}/api/v1/notes/${encodeURIComponent(id)}`, {
      method: "DELETE",
      headers: { ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }) },
    });
    const text = await upstream.text();
    return new NextResponse(text.length > 0 ? text : null, {
      status: upstream.status,
      headers: { "Content-Type": "application/json" },
    });
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : "Upstream request failed";
    return NextResponse.json({ error: message }, { status: 502 });
  }
}
