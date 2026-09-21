/**
 * Golden set proxy — POST /api/golden/queries/generate
 *   → POST /api/v1/golden/queries/generate
 *
 * No request body; the response only carries counts, so it is safe to
 * pass through verbatim.
 */
import { NextResponse } from "next/server";

export const maxDuration = 90;

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

export async function POST(): Promise<NextResponse> {
  try {
    const upstream = await fetch(`${BACKEND_URL}/api/v1/golden/queries/generate`, {
      method: "POST",
      signal: AbortSignal.timeout(75_000),
      headers: { ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }) },
    });
    const text = await upstream.text();
    return new NextResponse(text.length > 0 ? text : null, {
      status: upstream.status,
      headers: { "Content-Type": "application/json" },
    });
  } catch (error: unknown) {
    console.error(
      "[api/golden/queries/generate] upstream request failed:",
      error instanceof Error ? error.name : "unknown",
    );
    return NextResponse.json({ error: "upstream request failed" }, { status: 502 });
  }
}
