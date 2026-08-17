/**
 * Ask proxy — POST /api/ask
 *
 * Session-protected (web/src/proxy.ts catch-all). Unlike every other proxy
 * in this app, this handler does NOT buffer the upstream response with
 * .json()/.text() — it passes upstream.body straight through as the
 * Response body, preserving the SSE stream (spec §5.1). Buffering here
 * would silently turn a streaming answer into a "wait for everything, then
 * show it all at once" answer, with no error and no obvious symptom other
 * than a much longer time-to-first-token.
 */
import { type NextRequest } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

export async function POST(request: NextRequest): Promise<Response> {
  const body = await request.text();

  let upstream: Response;
  try {
    upstream = await fetch(`${BACKEND_URL}/api/v1/ask`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }),
      },
      body,
      // Forward client aborts (new question / navigation away) upstream so
      // the backend stops generating instead of continuing to burn tokens
      // for a client that has already stopped listening.
      signal: request.signal,
    });
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : "upstream unreachable";
    return new Response(JSON.stringify({ error: message }), {
      status: 502,
      headers: { "Content-Type": "application/json" },
    });
  }

  // A non-SSE error response (e.g. 401/400/503 per spec §5.1's error table)
  // is still JSON, not a stream — forward it as-is rather than trying to
  // stream a body that was never a stream.
  if (
    !upstream.body ||
    !(upstream.headers.get("content-type") ?? "").includes("text/event-stream")
  ) {
    const text = await upstream.text();
    return new Response(text, {
      status: upstream.status,
      headers: { "Content-Type": upstream.headers.get("content-type") ?? "application/json" },
    });
  }

  return new Response(upstream.body, {
    status: upstream.status,
    headers: {
      "Content-Type": "text/event-stream",
      "Cache-Control": "no-cache, no-transform",
      Connection: "keep-alive",
      // Disables response buffering in any reverse proxy sitting between
      // this handler and the browser (cloudflared, per spec §8) — without
      // this, an intermediate proxy can still batch the stream even though
      // this handler itself does not.
      "X-Accel-Buffering": "no",
    },
  });
}

// Never cache an SSE response — each call is a unique, stateful generation.
export const dynamic = "force-dynamic";
