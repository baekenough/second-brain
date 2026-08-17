/**
 * Upload proxy — POST /api/upload
 *
 * Session-protected route (NOT under /api/v1/*) that lets the Capture
 * screen upload a file to the backend's file-ingest endpoint. Unlike
 * src/app/api/v1/ingest/file/route.ts — which forwards the mobile app's
 * own Bearer token as-is — the browser has no API_KEY of its own, so this
 * route attaches the server-held API_KEY, mirroring
 * src/app/api/notes/route.ts's pattern.
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

const BACKEND_URL =
  process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
const API_KEY = process.env.API_KEY ?? "";

export async function POST(request: NextRequest): Promise<NextResponse> {
  // Content-Type must be forwarded verbatim for multipart (includes boundary)
  const contentType = request.headers.get("content-type") ?? "";
  const body = await request.arrayBuffer();

  try {
    const upstream = await fetch(`${BACKEND_URL}/api/v1/ingest/file`, {
      method: "POST",
      headers: {
        ...(contentType ? { "Content-Type": contentType } : {}),
        ...(API_KEY && { Authorization: `Bearer ${API_KEY}` }),
      },
      body,
    });
    const data: unknown = await upstream.json().catch(() => ({}));
    return NextResponse.json(data, { status: upstream.status });
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : "Upstream request failed";
    return NextResponse.json({ error: message }, { status: 502 });
  }
}
