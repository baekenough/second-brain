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

/** GET /api/graph/expand → proxies to GET /api/v1/graph/expand
 *
 * The upstream body carries entity names, so it is never logged (plan
 * §privacy 4) — only the transport error is. */
export async function GET(request: NextRequest): Promise<NextResponse> {
  try {
    const qs = request.nextUrl.searchParams.toString();
    const upstream = await fetch(`${BRAIN_API_URL}/api/v1/graph/expand${qs ? `?${qs}` : ""}`, {
      headers: authHeaders(),
    });
    const data: unknown = await upstream.json();
    return NextResponse.json(data, { status: upstream.status });
  } catch (err) {
    console.error("[api/graph/expand] upstream error:", err);
    return NextResponse.json({ error: "upstream error" }, { status: 502 });
  }
}
