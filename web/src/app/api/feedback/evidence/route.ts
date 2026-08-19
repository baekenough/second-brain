/**
 * Evidence feedback proxy — POST /api/feedback/evidence
 *   → POST /api/v1/feedback/evidence
 *
 * The upstream upsert is idempotent per (conversation, normalised query,
 * document), so a replayed request is safe; the /ask screen relies on that when
 * it retries after a failed optimistic update.
 *
 * Privacy: the body carries the user's own question and a document id for
 * personal material, and the upstream 400 messages deliberately never quote
 * either. Nothing from the body or the response is logged here — only the
 * transport error's name (plan §privacy). The question travels in the body, not
 * in a query string, so it never reaches an access log (cf. issue #208).
 *
 * A 404 from upstream (FEEDBACK_EVIDENCE_ENABLED off) is passed through
 * verbatim: the client reads it as "feature disabled" and hides the buttons.
 */
import { type NextRequest, NextResponse } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

interface EvidenceBody {
  conversation_id?: unknown;
  query?: unknown;
  document_id?: unknown;
  thumbs?: unknown;
  rank?: unknown;
  layer?: unknown;
}

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const LAYERS = new Set(["note", "observed", "insight"]);

export async function POST(request: NextRequest): Promise<NextResponse> {
  let body: EvidenceBody;
  try {
    body = (await request.json()) as EvidenceBody;
  } catch {
    // The parse error can quote the body, which contains the question.
    return NextResponse.json({ error: "invalid request body" }, { status: 400 });
  }

  const conversationId =
    typeof body.conversation_id === "string" ? body.conversation_id.trim() : "";
  const query = typeof body.query === "string" ? body.query.trim() : "";
  const documentId = typeof body.document_id === "string" ? body.document_id : "";
  const thumbs = body.thumbs;
  // Rejected here rather than forwarded so a malformed client cannot spend an
  // upstream round-trip. Messages never quote the offending value.
  if (conversationId === "") {
    return NextResponse.json({ error: "conversation_id is required" }, { status: 400 });
  }
  if (query === "") {
    return NextResponse.json({ error: "query is required" }, { status: 400 });
  }
  if (!UUID_RE.test(documentId)) {
    return NextResponse.json({ error: "document_id must be a UUID" }, { status: 400 });
  }
  if (thumbs !== 1 && thumbs !== 0 && thumbs !== -1) {
    return NextResponse.json({ error: "thumbs must be -1, 0, or 1" }, { status: 400 });
  }
  const rank = typeof body.rank === "number" && Number.isFinite(body.rank) ? body.rank : 0;
  const layer = typeof body.layer === "string" && LAYERS.has(body.layer) ? body.layer : "";

  try {
    const upstream = await fetch(`${BACKEND_URL}/api/v1/feedback/evidence`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }),
      },
      body: JSON.stringify({
        conversation_id: conversationId,
        query,
        document_id: documentId,
        thumbs,
        rank,
        layer,
      }),
    });
    const text = await upstream.text();
    return new NextResponse(text.length > 0 ? text : null, {
      status: upstream.status,
      headers: { "Content-Type": "application/json" },
    });
  } catch (error: unknown) {
    console.error(
      "[api/feedback/evidence] upstream request failed:",
      error instanceof Error ? error.name : "unknown",
    );
    return NextResponse.json({ error: "upstream request failed" }, { status: 502 });
  }
}
