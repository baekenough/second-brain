/**
 * Golden set proxy — POST /api/golden/judgments → POST /api/v1/golden/judgments
 *
 * The body carries the user's own labeling decisions tied to a query id and
 * document ids — personal search material — so nothing from the body or the
 * response is logged here, only the transport error's name.
 */
import { type NextRequest, NextResponse } from "next/server";

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

const JUDGMENTS = new Set(["relevant", "irrelevant", "noise"]);

interface JudgmentInputBody {
  document_id?: unknown;
  judgment?: unknown;
  rank?: unknown;
}

interface JudgmentsBody {
  query_id?: unknown;
  judgments?: unknown;
  finish_query?: unknown;
}

export async function POST(request: NextRequest): Promise<NextResponse> {
  let body: JudgmentsBody;
  try {
    body = (await request.json()) as JudgmentsBody;
  } catch {
    // The parse error can quote the body, which carries query text.
    return NextResponse.json({ error: "invalid request body" }, { status: 400 });
  }

  const queryId = typeof body.query_id === "string" ? body.query_id.trim() : "";
  if (queryId === "") {
    return NextResponse.json({ error: "query_id is required" }, { status: 400 });
  }
  if (!Array.isArray(body.judgments)) {
    return NextResponse.json({ error: "judgments must be an array" }, { status: 400 });
  }

  const judgments = [];
  for (const raw of body.judgments as JudgmentInputBody[]) {
    const documentId = typeof raw?.document_id === "string" ? raw.document_id : "";
    const judgment = raw?.judgment;
    const rank = typeof raw?.rank === "number" && Number.isFinite(raw.rank) ? raw.rank : 0;
    if (documentId === "") {
      return NextResponse.json({ error: "document_id is required" }, { status: 400 });
    }
    if (typeof judgment !== "string" || !JUDGMENTS.has(judgment)) {
      return NextResponse.json(
        { error: "judgment must be relevant, irrelevant, or noise" },
        { status: 400 },
      );
    }
    judgments.push({ document_id: documentId, judgment, rank });
  }

  const finishQuery = body.finish_query === true;

  try {
    const upstream = await fetch(`${BACKEND_URL}/api/v1/golden/judgments`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }),
      },
      body: JSON.stringify({ query_id: queryId, judgments, finish_query: finishQuery }),
    });
    const text = await upstream.text();
    return new NextResponse(text.length > 0 ? text : null, {
      status: upstream.status,
      headers: { "Content-Type": "application/json" },
    });
  } catch (error: unknown) {
    console.error(
      "[api/golden/judgments] upstream request failed:",
      error instanceof Error ? error.name : "unknown",
    );
    return NextResponse.json({ error: "upstream request failed" }, { status: 502 });
  }
}
