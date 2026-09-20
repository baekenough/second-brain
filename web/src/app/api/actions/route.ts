/**
 * Actions proxy — GET/POST /api/actions → GET/POST /api/v1/actions
 *
 * Authentication is already handled upstream of this handler by
 * web/src/proxy.ts (Cloudflare Access JWT); nothing auth-related belongs here.
 *
 * The response body carries action summaries and counterpart names, so it is
 * never logged and never echoed into an error body — only the fact that a
 * transport error happened (plan Task 8).
 */
import { type NextRequest, NextResponse } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

/** Forwarded query parameters. The list is a whitelist rather than a
 * pass-through of request.nextUrl.searchParams: forwarding everything turns a
 * front-end typo into a backend 400 that the user sees as a broken screen. */
const FORWARDED_PARAMS = [
  "kind",
  "counterpart",
  "due_before",
  "min_confidence",
  "sort",
  "limit",
  "include_archived",
] as const;

export async function GET(request: NextRequest): Promise<NextResponse> {
  if (request.nextUrl.searchParams.has("counterpart")) {
    return NextResponse.json({ error: "counterpart filter requires a POST body" }, { status: 400 });
  }
  return forwardActions(request.nextUrl.searchParams, "GET");
}

export async function POST(request: NextRequest): Promise<NextResponse> {
  if (request.nextUrl.searchParams.has("counterpart")) {
    return NextResponse.json({ error: "counterpart filter requires a POST body" }, { status: 400 });
  }
  if (
    request.headers.get("content-type")?.split(";")[0]?.trim().toLowerCase() !==
    "application/x-www-form-urlencoded"
  ) {
    return NextResponse.json({ error: "invalid action filters" }, { status: 400 });
  }
  try {
    const body = await request.text();
    if (new TextEncoder().encode(body).length > 4096) {
      return NextResponse.json({ error: "invalid action filters" }, { status: 400 });
    }
    return forwardActions(new URLSearchParams(body), "POST");
  } catch {
    return NextResponse.json({ error: "invalid action filters" }, { status: 400 });
  }
}

async function forwardActions(
  incoming: URLSearchParams,
  method: "GET" | "POST",
): Promise<NextResponse> {
  const forwarded = new URLSearchParams();
  for (const name of FORWARDED_PARAMS) {
    // kind repeats; getAll covers both the repeated and the single case.
    for (const value of incoming.getAll(name)) {
      if (value !== "") forwarded.append(name, value);
    }
  }
  const qs = forwarded.toString();

  try {
    const upstream = await fetch(
      `${BACKEND_URL}/api/v1/actions${method === "GET" && qs ? `?${qs}` : ""}`,
      {
        method,
        headers: {
          ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }),
          ...(method === "POST" && { "Content-Type": "application/x-www-form-urlencoded" }),
        },
        ...(method === "POST" && { body: qs }),
        cache: "no-store",
      },
    );
    const text = await upstream.text();
    return new NextResponse(text.length > 0 ? text : null, {
      status: upstream.status,
      headers: { "Content-Type": "application/json", "Cache-Control": "no-store" },
    });
  } catch (error: unknown) {
    // Only the error class name is logged. A fetch failure can quote the
    // request or response details, which may contain personal data.
    console.error(
      "[api/actions] upstream request failed:",
      error instanceof Error ? error.name : "unknown",
    );
    return NextResponse.json({ error: "upstream request failed" }, { status: 502 });
  }
}
