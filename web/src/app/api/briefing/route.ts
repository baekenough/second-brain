/**
 * Briefing proxy — GET /api/briefing → GET /api/v1/briefing
 *
 * Takes no query parameters: the briefing's inputs are the open action set and
 * the server-side cache key derived from it, not anything the client sends.
 *
 * The response body is a restatement of personal conversations, so it is never
 * logged (plan Task 8 — the rule this project already broke once, #194).
 */
import { NextResponse } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

export async function GET(): Promise<NextResponse> {
  try {
    const upstream = await fetch(`${BACKEND_URL}/api/v1/briefing`, {
      headers: { ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }) },
      cache: "no-store",
    });
    const text = await upstream.text();
    return new NextResponse(text.length > 0 ? text : null, {
      status: upstream.status,
      headers: { "Content-Type": "application/json" },
    });
  } catch (error: unknown) {
    console.error(
      "[api/briefing] upstream request failed:",
      error instanceof Error ? error.name : "unknown",
    );
    return NextResponse.json({ error: "upstream request failed" }, { status: 502 });
  }
}
