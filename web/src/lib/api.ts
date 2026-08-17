import type {
  BaselineStats,
  CreateNoteResponse,
  DocumentDetail,
  DocumentsResponse,
  IngestFileResponse,
  RecentItemsResponse,
  SearchParams,
  SearchResponse,
  SourcesResponse,
  StatsResponse,
} from "./types";

/** Per-upload size cap enforced by the backend (internal/api/ingest_file.go
 * defaultIngestMaxFileBytes) — mirrored here so the Capture screen can
 * reject oversized files before spending an upload round-trip. */
export const MAX_UPLOAD_FILE_BYTES = 100 * 1024 * 1024;

/**
 * Resolve the API base URL depending on execution environment.
 *
 * - Browser (client components): relative path → Next.js API proxy on same origin
 *   (cookie-based OAuth auth handled by the browser automatically).
 * - Node.js (server components / SSR): talk directly to the backend API server,
 *   bypassing the OAuth-gated Next.js proxy.  The page is already protected by
 *   proxy.ts middleware, so a logged-out user never reaches the Server Component;
 *   using API_KEY here is safe.
 *   Path prefix is /api/v1 because the backend routes are mounted there, and the
 *   relative sub-paths used by each helper (e.g. /documents, /search) are the
 *   same as the /api/* proxy paths they mirror.
 */
function getApiBase(): string {
  if (typeof window !== "undefined") {
    return "/api";
  }
  const backendBase =
    process.env.BRAIN_API_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:9200";
  return `${backendBase}/api/v1`;
}

async function fetchJson<T>(url: string, init?: RequestInit): Promise<T> {
  let options = init;
  // On the server, authenticate directly against the backend with the API key.
  // On the client, the browser cookie carries the session — never expose the key.
  if (typeof window === "undefined" && process.env.API_KEY) {
    const authHeader = { Authorization: `Bearer ${process.env.API_KEY}` };
    options = {
      ...init,
      headers: {
        ...init?.headers,
        ...authHeader,
      },
    };
  }
  const response = await fetch(url, options);
  if (!response.ok) {
    throw new Error(`API error: ${response.status} ${response.statusText}`);
  }
  return response.json() as Promise<T>;
}

// ── Search ────────────────────────────────────────────────────────────────

export async function searchDocuments(params: SearchParams): Promise<SearchResponse> {
  const body = {
    query: params.query,
    ...(params.source_type && params.source_type !== "all" && { source_type: params.source_type }),
    ...(params.exclude_source_types?.length && {
      exclude_source_types: params.exclude_source_types,
    }),
    ...(params.limit !== undefined && { limit: params.limit }),
    ...(params.offset !== undefined && { offset: params.offset }),
    ...(params.sort && params.sort !== "relevance" && { sort: params.sort }),
    ...(params.use_hyde && { use_hyde: true }),
    ...(params.use_rerank && { use_rerank: true }),
    ...(params.curated && { curated: true }),
    ...(params.include_deleted && { include_deleted: true }),
  };

  return fetchJson<SearchResponse>(`${getApiBase()}/search`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

// ── Documents ─────────────────────────────────────────────────────────────

export async function getDocument(id: string): Promise<DocumentDetail> {
  return fetchJson<DocumentDetail>(`${getApiBase()}/documents/${encodeURIComponent(id)}`);
}

export async function listDocuments(options?: {
  source?: string;
  excludeSource?: string;
  limit?: number;
  offset?: number;
}): Promise<DocumentDetail[]> {
  const params = new URLSearchParams();
  if (options?.source) params.set("source", options.source);
  if (options?.excludeSource) params.set("exclude_source", options.excludeSource);
  if (options?.limit !== undefined) params.set("limit", String(options.limit));
  if (options?.offset !== undefined) params.set("offset", String(options.offset));

  const qs = params.toString();
  const res = await fetchJson<DocumentsResponse>(`${getApiBase()}/documents${qs ? `?${qs}` : ""}`);
  return res.documents ?? [];
}

export async function listRecentDocuments(
  limit = 10,
  source?: string,
  excludeSources?: string[],
): Promise<DocumentDetail[]> {
  return listDocuments({
    limit,
    source,
    excludeSource: excludeSources?.join(","),
  });
}

export async function listRecentByKind(
  kind: "sms" | "call-recording" | "voice-memo",
  limit = 50,
): Promise<RecentItemsResponse> {
  const params = new URLSearchParams({ kind, limit: String(limit) });
  return fetchJson<RecentItemsResponse>(`${getApiBase()}/documents/recent?${params}`);
}

// ── Stats ─────────────────────────────────────────────────────────────────

export async function getStats(): Promise<StatsResponse> {
  return fetchJson<StatsResponse>(`${getApiBase()}/stats`);
}

export async function getBaselineStats(): Promise<BaselineStats> {
  return fetchJson<BaselineStats>(`${getApiBase()}/stats/baseline`);
}

// ── Sources ───────────────────────────────────────────────────────────────

export async function getSources(): Promise<SourcesResponse> {
  return fetchJson<SourcesResponse>(`${getApiBase()}/sources`);
}

// ── Notes (Capture) ──────────────────────────────────────────────────────

export async function createNote(content: string, title = ""): Promise<CreateNoteResponse> {
  return fetchJson<CreateNoteResponse>(`${getApiBase()}/notes`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ title, content }),
  });
}

export async function retryNoteEnrichment(id: string): Promise<void> {
  await fetchJson<unknown>(`${getApiBase()}/notes/${encodeURIComponent(id)}/retry-enrichment`, {
    method: "POST",
  });
}

export async function deleteNote(id: string): Promise<void> {
  await fetchJson<unknown>(`${getApiBase()}/notes/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

// ── File upload (Capture) ────────────────────────────────────────────────

/** Uploads a file to POST /api/upload (browser-only). No Content-Type is
 * set explicitly — the browser derives the multipart boundary from the
 * FormData body automatically; setting it manually would drop the boundary. */
export async function uploadFile(file: File): Promise<IngestFileResponse> {
  const formData = new FormData();
  formData.append("file", file);
  return fetchJson<IngestFileResponse>(`${getApiBase()}/upload`, {
    method: "POST",
    body: formData,
  });
}
