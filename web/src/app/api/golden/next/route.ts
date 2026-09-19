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
  [key: string]: unknown;
}

interface NextBody {
  candidates?: unknown;
  [key: string]: unknown;
}

/**
 * Normalises `candidates[].retention === ""` to `null` (coordinator
 * confirmation, 2026-09-19): the backend may send an empty string for an
 * untagged document, and the frontend type (RetentionTag) only distinguishes
 * "keep" | "low" | "disposable" | null — an empty string sliding through
 * unnormalised would fail every `retention === null` check on the client and
 * silently render as neither a known tag nor "미태그".
 *
 * Parse failures and non-2xx bodies pass through untouched — this function is
 * only ever called after `upstream.ok` is confirmed, and a shape mismatch
 * inside a 2xx body degrades to "leave it as-is" rather than throwing, since
 * a proxy-side bug here must never block the underlying successful response.
 */
export function normalizeRetention(text: string): string {
  let body: NextBody;
  try {
    body = JSON.parse(text) as NextBody;
  } catch {
    return text;
  }
  if (!Array.isArray(body.candidates)) {
    return text;
  }
  for (const candidate of body.candidates as CandidateBody[]) {
    if (candidate && candidate.retention === "") {
      candidate.retention = null;
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
    const body = upstream.ok && text.length > 0 ? normalizeRetention(text) : text;
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
