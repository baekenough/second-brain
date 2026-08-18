/**
 * Actions proxy — POST /api/actions/{key}/status
 *   → POST /api/v1/actions/{identity_key}/status
 *
 * The upstream endpoint is idempotent, so a replayed request is safe; the /actions
 * screen relies on that when it retries a failed optimistic update.
 *
 * The request body can contain a user note and the response echoes the
 * identity key, so neither is logged (plan Task 8).
 */
import { type NextRequest, NextResponse } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

/** Shape of an identity key ("action:" + 16 hex), mirroring
 * internal/api/actions.go. The value is a deterministic hash that
 * action_status rows already point at: it is validated for SHAPE and passed
 * through unchanged, never normalised or recomputed (spec §5.5). */
const IDENTITY_KEY_RE = /^action:[0-9a-f]{16}$/;

interface StatusBody {
  state?: unknown;
  note?: unknown;
}

export async function POST(
  request: NextRequest,
  { params }: { params: Promise<{ key: string }> },
): Promise<NextResponse> {
  // Next decodes the dynamic segment, so the "action:" colon arrives intact.
  const { key } = await params;
  if (!IDENTITY_KEY_RE.test(key)) {
    return NextResponse.json({ error: "invalid action key" }, { status: 400 });
  }

  let body: StatusBody;
  try {
    body = (await request.json()) as StatusBody;
  } catch {
    // The parse error can quote the body, which may contain a note.
    return NextResponse.json({ error: "invalid request body" }, { status: 400 });
  }

  const state = body.state;
  if (state !== "open" && state !== "done" && state !== "ignored") {
    return NextResponse.json({ error: "invalid state" }, { status: 400 });
  }
  const note = typeof body.note === "string" ? body.note : "";

  try {
    const upstream = await fetch(
      `${BACKEND_URL}/api/v1/actions/${encodeURIComponent(key)}/status`,
      {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }),
        },
        body: JSON.stringify({ state, note }),
      },
    );
    const text = await upstream.text();
    return new NextResponse(text.length > 0 ? text : null, {
      status: upstream.status,
      headers: { "Content-Type": "application/json" },
    });
  } catch (error: unknown) {
    console.error(
      "[api/actions/status] upstream request failed:",
      error instanceof Error ? error.name : "unknown",
    );
    return NextResponse.json({ error: "upstream request failed" }, { status: 502 });
  }
}
