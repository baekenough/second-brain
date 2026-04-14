import { NextRequest, NextResponse } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

export async function GET(
  _request: NextRequest,
  { params }: { params: Promise<{ id: string }> }
) {
  const { id } = await params;
  const upstreamUrl = `${BACKEND_URL}/api/v1/documents/${encodeURIComponent(id)}/raw`;

  try {
    const upstream = await fetch(upstreamUrl, {
      headers: {
        ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }),
      },
      cache: "no-store",
    });

    if (!upstream.ok) {
      const text = await upstream.text();
      return NextResponse.json(
        { error: text || upstream.statusText },
        { status: upstream.status }
      );
    }

    const contentType = upstream.headers.get("content-type") ?? "application/octet-stream";
    const cacheControl = upstream.headers.get("cache-control") ?? "public, max-age=300";
    return new NextResponse(upstream.body, {
      status: 200,
      headers: {
        "Content-Type": contentType,
        "Cache-Control": cacheControl,
      },
    });
  } catch (err: unknown) {
    const message = err instanceof Error ? err.message : "Upstream failed";
    return NextResponse.json({ error: message }, { status: 502 });
  }
}
