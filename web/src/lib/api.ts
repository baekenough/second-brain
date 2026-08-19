import type {
  ActionListParams,
  ActionsResponse,
  ActionState,
  AskConversationSummary,
  AskConversationTurn,
  BaselineStats,
  BriefingResponse,
  CreateNoteResponse,
  DocumentDetail,
  DocumentsResponse,
  EvidenceFeedbackRequest,
  EvidenceFeedbackResponse,
  EvidenceVote,
  GraphEntitiesResponse,
  GraphEntityHit,
  GraphEntryResponse,
  GraphEvidence,
  GraphEvidenceResponse,
  GraphExpandResponse,
  GraphFilters,
  GraphNeighbor,
  GraphNode,
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

// ── Ask (conversations) ──────────────────────────────────────────────────
//
// Both helpers are client-only: they hit /api/ask/conversations[/:id], the
// session-protected proxy pair (web/src/app/api/ask/conversations/*), not
// the backend's /api/v1/ask/conversations directly. getApiBase()'s server
// branch would prepend /api/v1, which does not exist under this proxy —
// these are only ever called from "use client" components (the /ask page).

export async function listConversations(limit = 20): Promise<AskConversationSummary[]> {
  const res = await fetchJson<AskConversationSummary[]>(
    `${getApiBase()}/ask/conversations?limit=${limit}`,
  );
  return res ?? [];
}

export async function getConversation(id: string): Promise<AskConversationTurn[]> {
  return fetchJson<AskConversationTurn[]>(
    `${getApiBase()}/ask/conversations/${encodeURIComponent(id)}`,
  );
}

// ── Knowledge graph (Part B) ─────────────────────────────────────────────
//
// Browser-only helpers: they hit the /api/graph/* proxy pair
// (web/src/app/api/graph/*), which forwards to the backend's
// /api/v1/graph/*. The backend clamps every limit, so anything sent here is
// a hint, not a guarantee.
//
// No response body is ever logged — entity names would leak into the browser
// console (plan §privacy 4).

/** Thrown when the backend answers 503: Neo4j is a derived store that can be
 * down while search/ask stay healthy, and the page must say so specifically
 * instead of showing a generic failure. */
export class GraphUnavailableError extends Error {
  constructor() {
    super("graph temporarily unavailable");
    this.name = "GraphUnavailableError";
  }
}

async function fetchGraph<T>(path: string, params: URLSearchParams): Promise<T> {
  const qs = params.toString();
  const response = await fetch(`${getApiBase()}/graph/${path}${qs ? `?${qs}` : ""}`);
  if (response.status === 503) {
    throw new GraphUnavailableError();
  }
  if (!response.ok) {
    throw new Error(`API error: ${response.status} ${response.statusText}`);
  }
  return response.json() as Promise<T>;
}

function baseGraphParams(f: GraphFilters): URLSearchParams {
  const params = new URLSearchParams({
    days: String(f.days),
    min_confidence: String(f.minConfidence),
  });
  return params;
}

export async function getGraphEntry(f: GraphFilters, limit?: number): Promise<GraphNode[]> {
  const params = baseGraphParams(f);
  if (f.entityTypes.length > 0) params.set("types", f.entityTypes.join(","));
  if (limit !== undefined) params.set("limit", String(limit));
  const res = await fetchGraph<GraphEntryResponse>("entry", params);
  return res.nodes ?? [];
}

export async function expandGraphNode(
  entityId: number,
  f: GraphFilters,
  limit?: number,
): Promise<GraphNeighbor[]> {
  const params = baseGraphParams(f);
  params.set("entity_id", String(entityId));
  if (f.relTypes.length > 0) params.set("rel_types", f.relTypes.join(","));
  if (limit !== undefined) params.set("limit", String(limit));
  const res = await fetchGraph<GraphExpandResponse>("expand", params);
  return res.neighbors ?? [];
}

export async function getGraphEvidence(
  from: number,
  to: number,
  relType: string,
): Promise<GraphEvidence[]> {
  const params = new URLSearchParams({
    from: String(from),
    to: String(to),
    rel_type: relType,
  });
  const res = await fetchGraph<GraphEvidenceResponse>("evidence", params);
  return res.evidence ?? [];
}

export async function searchGraphEntities(q: string): Promise<GraphEntityHit[]> {
  const res = await fetchGraph<GraphEntitiesResponse>("entities", new URLSearchParams({ q }));
  return res.entities ?? [];
}

// ── Actions & briefing (Part C) ──────────────────────────────────────────
//
// Browser-only helpers: they hit the /api/actions and /api/briefing proxy
// routes (web/src/app/api/actions/*, web/src/app/api/briefing), which forward
// to the backend's /api/v1/*.
//
// No response body is ever logged — an action summary and a counterpart name
// are personal data (plan Task 8).

/** Thrown when the backend answers 404 for the actions list, which is what the
 * ACTIONS_API_ENABLED flag being off looks like from here: the route is not
 * registered at all. The screen says "feature disabled" instead of showing a
 * generic failure. */
export class ActionsDisabledError extends Error {
  constructor() {
    super("actions feature disabled");
    this.name = "ActionsDisabledError";
  }
}

export async function listActions(params: ActionListParams = {}): Promise<ActionsResponse> {
  const qs = new URLSearchParams();
  for (const kind of params.kinds ?? []) qs.append("kind", kind);
  if (params.counterpart) qs.set("counterpart", params.counterpart);
  if (params.dueBefore) qs.set("due_before", params.dueBefore);
  // 0 is the documented "no floor" default; sending it would only add noise.
  if (params.minConfidence !== undefined && params.minConfidence > 0) {
    qs.set("min_confidence", String(params.minConfidence));
  }
  if (params.sort) qs.set("sort", params.sort);
  if (params.limit !== undefined) qs.set("limit", String(params.limit));
  if (params.includeArchived) qs.set("include_archived", "true");

  const search = qs.toString();
  const response = await fetch(`${getApiBase()}/actions${search ? `?${search}` : ""}`);
  if (response.status === 404) {
    throw new ActionsDisabledError();
  }
  if (!response.ok) {
    throw new Error(`API error: ${response.status} ${response.statusText}`);
  }
  return response.json() as Promise<ActionsResponse>;
}

/** Marks one action done or ignored. The backend is idempotent, so replaying
 * this call after a failed attempt is safe — which is what makes the
 * optimistic update on the /actions screen recoverable.
 *
 * A 404 here is NOT mapped to ActionsDisabledError: the backend answers 404
 * both for an unregistered route and for an unknown identity_key, and the two
 * are indistinguishable from the client. */
export async function setActionState(
  identityKey: string,
  state: Extract<ActionState, "done" | "ignored">,
  note?: string,
): Promise<void> {
  const response = await fetch(
    `${getApiBase()}/actions/${encodeURIComponent(identityKey)}/status`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ state, ...(note ? { note } : {}) }),
    },
  );
  if (!response.ok) {
    throw new Error(`API error: ${response.status} ${response.statusText}`);
  }
}

export async function getBriefing(): Promise<BriefingResponse> {
  const response = await fetch(`${getApiBase()}/briefing`);
  if (!response.ok) {
    throw new Error(`API error: ${response.status} ${response.statusText}`);
  }
  return response.json() as Promise<BriefingResponse>;
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

// ── Evidence feedback (Part D) ───────────────────────────────────────────
//
// Browser-only. Hits the /api/feedback/evidence proxy route
// (web/src/app/api/feedback/evidence/route.ts), which forwards to the
// backend's POST /api/v1/feedback/evidence.
//
// Neither the request nor the response is ever logged: the body carries the
// user's own question and a document id for personal material.

/** Thrown when the endpoint answers 404, which is what FEEDBACK_EVIDENCE_ENABLED
 * being off looks like from here — the handler returns 404 before reading the
 * body, precisely so a disabled feature is indistinguishable from an absent
 * one. The caller hides the thumbs buttons instead of showing an error. */
export class EvidenceFeedbackDisabledError extends Error {
  constructor() {
    super("evidence feedback disabled");
    this.name = "EvidenceFeedbackDisabledError";
  }
}

/** Records one thumbs judgement on one evidence card and returns the value the
 * *server* settled on.
 *
 * The upstream upsert is idempotent per (conversation, normalised query,
 * document) and toggles a repeated identical vote back to 0, so replaying this
 * call is safe — which is what makes an optimistic UI update recoverable.
 *
 * The question is sent in the POST body, never as a query parameter: it is
 * personal text and a query string is logged by every hop in between. */
export async function sendEvidenceFeedback(
  request: EvidenceFeedbackRequest,
): Promise<EvidenceVote> {
  const response = await fetch(`${getApiBase()}/feedback/evidence`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(request),
  });
  if (response.status === 404) {
    throw new EvidenceFeedbackDisabledError();
  }
  if (!response.ok) {
    // The status line only; the body could echo validation detail.
    throw new Error(`API error: ${response.status} ${response.statusText}`);
  }
  const data = (await response.json()) as EvidenceFeedbackResponse;
  // Defensive: an unexpected value must not become the rendered state.
  return data.thumbs === 1 || data.thumbs === -1 ? data.thumbs : 0;
}
