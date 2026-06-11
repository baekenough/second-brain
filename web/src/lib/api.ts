import type {
  BaselineStats,
  DocumentDetail,
  DocumentsResponse,
  RecentItemsResponse,
  SearchParams,
  SearchResponse,
  SourcesResponse,
  StatsResponse,
} from "./types";

/**
 * Resolve the API base URL depending on execution environment.
 *
 * - Browser (client components): relative path → Next.js API proxy on same origin.
 * - Node.js (server components / SSR): fetch needs an absolute URL.
 *   We route through the Next.js proxy running on APP_URL so that the
 *   proxy's server-side env vars (BRAIN_API_URL, API_KEY) are applied.
 */
function getApiBase(): string {
  if (typeof window !== "undefined") {
    return "/api";
  }
  const appUrl =
    process.env.NEXT_PUBLIC_APP_URL ?? `http://localhost:${process.env.PORT ?? "3000"}`;
  return `${appUrl}/api`;
}

async function fetchJson<T>(url: string, init?: RequestInit): Promise<T> {
  const response = await fetch(url, init);
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
