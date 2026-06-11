import { type NextRequest, NextResponse } from "next/server";

const BRAIN_API_URL = process.env.BRAIN_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

function authHeaders(): HeadersInit {
  const headers: Record<string, string> = {};
  if (API_KEY) {
    headers["Authorization"] = `Bearer ${API_KEY}`;
  }
  return headers;
}

/** GET /api/documents/recent → proxies to GET /api/v1/documents/recent */
export async function GET(request: NextRequest): Promise<NextResponse> {
  try {
    const qs = request.nextUrl.searchParams.toString();
    const upstream = await fetch(`${BRAIN_API_URL}/api/v1/documents/recent${qs ? `?${qs}` : ""}`, {
      headers: authHeaders(),
    });
    const data: unknown = await upstream.json();
    return NextResponse.json(data, { status: upstream.status });
  } catch (err) {
    console.error("[api/documents/recent] upstream error:", err);
    return NextResponse.json({ error: "upstream error" }, { status: 502 });
  }
}
