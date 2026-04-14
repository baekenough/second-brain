import { NextRequest, NextResponse } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

async function proxySearch(body: Record<string, unknown>) {
  const upstreamUrl = `${BACKEND_URL}/api/v1/search`;

  try {
    const response = await fetch(upstreamUrl, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }),
      },
      body: JSON.stringify(body),
      next: { revalidate: 0 },
    });

    const data: unknown = await response.json();
    return NextResponse.json(data, { status: response.status });
  } catch (error: unknown) {
    const message =
      error instanceof Error ? error.message : "Upstream request failed";
    return NextResponse.json({ error: message }, { status: 502 });
  }
}

export async function GET(request: NextRequest) {
  const { searchParams } = request.nextUrl;
  const query = searchParams.get("q") ?? searchParams.get("query") ?? "";
  const limitStr = searchParams.get("limit");
  const sourceType = searchParams.get("source_type");
  const sort = searchParams.get("sort");
  const body: Record<string, unknown> = { query };
  if (limitStr) {
    const n = Number.parseInt(limitStr, 10);
    if (Number.isFinite(n) && n > 0) body.limit = n;
  }
  if (sourceType) body.source_type = sourceType;
  if (sort === "relevance" || sort === "recent") body.sort = sort;
  const excludeStr = searchParams.get("exclude_source_types");
  if (excludeStr) {
    body.exclude_source_types = excludeStr.split(",").filter(Boolean);
  }
  return proxySearch(body);
}

export async function POST(request: NextRequest) {
  const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
  return proxySearch(body);
}
