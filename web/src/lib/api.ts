import type { DocumentDetail, SearchParams, SearchResponse, SourceType, StatsResponse } from "./types";

/**
 * Resolve the API base URL depending on execution environment.
 *
 * - Browser (client components): use a relative path so the request goes to
 *   the Next.js API proxy on the same origin.
 * - Node.js (server components / SSR): fetch needs an absolute URL.
 *   We route through the Next.js proxy running on APP_URL so that the
 *   proxy's server-side env vars (BRAIN_API_URL, API_KEY) are applied.
 */
function getApiBase(): string {
  if (typeof window !== "undefined") {
    // Client-side: relative path is fine
    return "/api";
  }
  // Server-side: must use an absolute URL pointing to the Next.js server itself
  const appUrl =
    process.env.NEXT_PUBLIC_APP_URL ??
    `http://localhost:${process.env.PORT ?? "3000"}`;
  return `${appUrl}/api`;
}

async function fetchJson<T>(url: string, init?: RequestInit): Promise<T> {
  const response = await fetch(url, init);
  if (!response.ok) {
    throw new Error(`API error: ${response.status} ${response.statusText}`);
  }
  return response.json() as Promise<T>;
}

export async function searchDocuments(
  params: SearchParams
): Promise<SearchResponse> {
  const searchParams = new URLSearchParams({
    query: params.query,
    ...(params.source_type &&
      params.source_type !== "all" && { source_type: params.source_type }),
    ...(params.limit !== undefined && { limit: String(params.limit) }),
    ...(params.offset !== undefined && { offset: String(params.offset) }),
    ...(params.sort && params.sort !== "relevance" && { sort: params.sort }),
  });

  if (params.exclude_source_types && params.exclude_source_types.length > 0) {
    searchParams.set(
      "exclude_source_types",
      params.exclude_source_types.join(",")
    );
  }

  return fetchJson<SearchResponse>(`${getApiBase()}/search?${searchParams}`);
}

export async function getDocument(id: string): Promise<DocumentDetail> {
  return fetchJson<DocumentDetail>(
    `${getApiBase()}/documents/${encodeURIComponent(id)}`
  );
}

export async function listRecentDocuments(
  limit = 10,
  source?: string,
  excludeSources?: SourceType[]
): Promise<DocumentDetail[]> {
  const params = new URLSearchParams({ limit: String(limit) });
  if (source) params.set("source", source);
  if (excludeSources && excludeSources.length > 0) {
    params.set("exclude_source", excludeSources.join(","));
  }
  const response = await fetchJson<{ documents: DocumentDetail[] }>(
    `${getApiBase()}/documents?${params}`
  );
  return response.documents ?? [];
}

export async function getStats(): Promise<StatsResponse> {
  return fetchJson<StatsResponse>(`${getApiBase()}/stats`);
}
