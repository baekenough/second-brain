/**
 * Golden set proxy — GET /api/golden/next → GET /api/v1/golden/next
 *
 * Authentication is already handled upstream of this handler by
 * web/src/proxy.ts (Cloudflare Access JWT); nothing auth-related belongs here.
 *
 * The response carries the user's own search queries and candidate document
 * titles/snippets, so it is never logged and never echoed into an error body
 * — only the fact that a transport error happened.
 */
import { type NextRequest, NextResponse } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

const DEFAULT_LIMIT = 10;
const MAX_LIMIT = 50;

interface CandidateBody {
  retention?: unknown;
  stream?: unknown;
  [key: string]: unknown;
}

interface QueryBody {
  window?: unknown;
  [key: string]: unknown;
}

interface NextBody {
  query?: unknown;
  candidates?: unknown;
  [key: string]: unknown;
}

const KNOWN_STREAMS = new Set(["relevance", "recent"]);

/**
 * Normalises the golden/next response body (coordinator confirmation,
 * 2026-09-19):
 * - `candidates[].retention === ""` → `null` (untagged document; the
 *   frontend type only distinguishes "keep" | "low" | "disposable" | null and
 *   an empty string would fail every `retention === null` check downstream).
 * - `candidates[].stream` missing/unrecognised → `"relevance"` (the older
 *   retrieval lane; only `"recent"` is a newly introduced value, so anything
 *   else defaults to the lane that already existed before this field shipped).
 * - `query.window` missing → `null` (older backend responses/tests may omit
 *   the field entirely; `null` is already the "no period expression" case the
 *   client renders, so a missing key collapses into the same state).
 *
 * Parse failures and non-2xx bodies pass through untouched — this function is
 * only ever called after `upstream.ok` is confirmed, and a shape mismatch
 * inside a 2xx body degrades to "leave it as-is" rather than throwing, since
 * a proxy-side bug here must never block the underlying successful response.
 */
export function normalizeGoldenNext(text: string): string {
  let body: NextBody;
  try {
    body = JSON.parse(text) as NextBody;
  } catch {
    return text;
  }

  if (body.query && typeof body.query === "object") {
    const query = body.query as QueryBody;
    if (query.window === undefined) {
      query.window = null;
    }
  }

  if (Array.isArray(body.candidates)) {
    for (const candidate of body.candidates as CandidateBody[]) {
      if (!candidate || typeof candidate !== "object") continue;
      if (candidate.retention === "") {
        candidate.retention = null;
      }
      if (typeof candidate.stream !== "string" || !KNOWN_STREAMS.has(candidate.stream)) {
        candidate.stream = "relevance";
      }
    }
  }

  return JSON.stringify(body);
}

export async function GET(request: NextRequest): Promise<NextResponse> {
  const rawLimit = request.nextUrl.searchParams.get("limit");
  const parsed = rawLimit ? Number(rawLimit) : DEFAULT_LIMIT;
  const limit =
    Number.isFinite(parsed) && parsed > 0 ? Math.min(Math.trunc(parsed), MAX_LIMIT) : DEFAULT_LIMIT;

  try {
    const upstream = await fetch(`${BACKEND_URL}/api/v1/golden/next?limit=${limit}`, {
      headers: { ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }) },
      cache: "no-store",
    });
    const text = await upstream.text();
    const body = upstream.ok && text.length > 0 ? normalizeGoldenNext(text) : text;
    return new NextResponse(body.length > 0 ? body : null, {
      status: upstream.status,
      headers: { "Content-Type": "application/json" },
    });
  } catch (error: unknown) {
    console.error(
      "[api/golden/next] upstream request failed:",
      error instanceof Error ? error.name : "unknown",
    );
    return NextResponse.json({ error: "upstream request failed" }, { status: 502 });
  }
}
