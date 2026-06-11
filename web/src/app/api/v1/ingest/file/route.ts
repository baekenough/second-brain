/**
 * Ingest proxy — POST /api/v1/ingest/file
 *
 * Pass-through to the backend ingest endpoint.
 * Authentication: forwards the mobile app's Bearer API_KEY header as-is.
 *   (This route is excluded from OAuth middleware — see middleware.ts)
 *
 * Request body (multipart/form-data, forwarded verbatim):
 *   file    — file to ingest (required)
 *   title   — document title (optional)
 *   source  — source type string (optional)
 *   tags    — comma-separated tag list (optional)
 *
 * Response:
 *   201 { document_id: string, accepted: boolean }
 */
import { type NextRequest, NextResponse } from "next/server";

function backendUrl(path: string): string {
  const base =
    process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
  return `${base.replace(/\/$/, "")}${path}`;
}

export async function POST(req: NextRequest): Promise<NextResponse> {
  const authorization = req.headers.get("authorization") ?? "";
  // Content-Type must be forwarded verbatim for multipart (includes boundary)
  const contentType = req.headers.get("content-type") ?? "";

  // Buffer the multipart body to forward it intact
  const body = await req.arrayBuffer();

  let upstream: Response;
  try {
    upstream = await fetch(backendUrl("/api/v1/ingest/file"), {
      method: "POST",
      headers: {
        Authorization: authorization,
        ...(contentType ? { "Content-Type": contentType } : {}),
      },
      body,
    });
  } catch (err) {
    const message = err instanceof Error ? err.message : "upstream error";
    return NextResponse.json({ error: message }, { status: 502 });
  }

  // Forward upstream body and status code verbatim
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": "application/json" },
  });
}
