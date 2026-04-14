import { NextRequest, NextResponse } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ??
  process.env.NEXT_PUBLIC_API_URL ??
  "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

export async function GET(request: NextRequest) {
  const { searchParams } = request.nextUrl;
  const limit = searchParams.get("limit") ?? "20";
  const offset = searchParams.get("offset") ?? "0";
  const source = searchParams.get("source");

  const url = new URL(`${BACKEND_URL}/api/v1/documents`);
  url.searchParams.set("limit", limit);
  url.searchParams.set("offset", offset);
  if (source) url.searchParams.set("source", source);
  const excludeSource = searchParams.get("exclude_source");
  if (excludeSource) url.searchParams.set("exclude_source", excludeSource);

  try {
    const upstream = await fetch(url.toString(), {
      headers: {
        "Content-Type": "application/json",
        ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }),
      },
      cache: "no-store",
    });
    const data = (await upstream.json()) as unknown;
    return NextResponse.json(data, { status: upstream.status });
  } catch (err: unknown) {
    const message = err instanceof Error ? err.message : "Upstream failed";
    return NextResponse.json({ error: message }, { status: 502 });
  }
}
