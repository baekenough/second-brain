/**
 * Source types mirror model.SourceType in the Go backend (internal/model/document.go).
 * Keep in sync when new source types are added.
 */
export type SourceType =
  | "slack"
  | "github"
  | "gdrive"
  | "notion"
  | "filesystem"
  | "discord"
  | "telegram"
  | "secretary"
  | "llm-memory"
  | "gmail"
  | "calendar"
  | "sms"
  | "call-log"
  | "call-transcript"
  | "upload"
  | "note"
  | "insight";

export type MatchType = "fulltext" | "vector" | "hybrid";

export interface DocumentMetadata {
  source_type?: SourceType;
  source_url?: string;
  channel?: string;
  repo?: string;
  folder?: string;
  author?: string;
  path?: string;
  ext?: string;
  phone_number?: string;
  contact_name?: string;
  direction?: "incoming" | "outgoing";
  duration_ms?: number;
  occurred_at?: string;
  [key: string]: string | number | boolean | undefined;
}

export interface SearchResultItem {
  id: string;
  title: string;
  content: string;
  source_type: SourceType;
  source_id: string;
  match_type: MatchType;
  score: number;
  status: string;
  collected_at: string;
  created_at: string;
  updated_at: string;
  metadata: DocumentMetadata;
}

export interface SearchResponse {
  results: SearchResultItem[];
  curated?: CuratedResult[];
  count: number;
  total: number;
  query: string;
  took_ms: number;
  is_curated?: boolean;
}

export interface CuratedResult {
  summary: string;
  relevance: number;
  relevance_reason: string;
  original: DocumentDetail;
}

export interface DocumentDetail {
  id: string;
  title: string;
  content: string;
  source_type: SourceType;
  source_id: string;
  status: string;
  collected_at: string;
  created_at: string;
  updated_at: string;
  metadata: DocumentMetadata;
}

/**
 * Metadata shape specific to source_type=note documents (spec §6.1).
 * Deliberately not extending DocumentMetadata — its index signature
 * (string | number | boolean | undefined) cannot accommodate
 * enrichment_last_error's `string | null`, and note metadata does not
 * overlap with DocumentMetadata's other fields (channel, repo, folder, ...).
 */
export interface NoteMetadata {
  enrichment_status?: "pending" | "done" | "failed";
  enrichment_attempts?: number;
  enrichment_last_error?: string | null;
  [key: string]: unknown;
}

/** Reads a DocumentDetail's metadata as NoteMetadata in one place, so call
 * sites never need an inline `as NoteMetadata` cast (spec §6.1). */
export function getNoteMetadata(doc: DocumentDetail): NoteMetadata {
  return (doc.metadata ?? {}) as NoteMetadata;
}

/** Response shape of POST /api/v1/notes (spec §6.1) — the note document
 * itself is not returned; the caller already has the content it just sent. */
export interface CreateNoteResponse {
  id: string;
  status: "pending";
}

/** Response shape of POST /api/v1/ingest/file (internal/api/ingest_file.go). */
export interface IngestFileResponse {
  document_id: string;
  accepted: boolean;
}

/** One entry of the `sources` SSE event (spec §5.1). */
export interface AskSourceItem {
  id: string;
  title: string;
  source_type: SourceType;
  score: number;
}

/** Parsed, typed form of the five /api/v1/ask SSE event types (spec §5.1).
 * "conversation" is always the first event of a stream — see
 * internal/api/ask_conversations.go's askConversationPayload doc comment. */
export type AskStreamEvent =
  | { type: "conversation"; conversation_id: string; turn_index: number }
  | { type: "sources"; sources: AskSourceItem[] }
  | { type: "token"; text: string }
  | { type: "done"; finish_reason: "stop" | "error" | "no_evidence" }
  | { type: "error"; message: string };

/** One element of GET /api/ask/conversations (backend:
 * askConversationSummary, internal/api/ask_conversations.go) — the latest
 * turn of a conversation, used to render the recent-conversations list. */
export interface AskConversationSummary {
  conversation_id: string;
  turn_index: number;
  question: string;
  answer: string;
  finish_reason: "stop" | "error" | "no_evidence";
  created_at: string;
}

/** One element of GET /api/ask/conversations/{id} (backend:
 * askConversationTurn, internal/api/ask_conversations.go) — a full turn
 * including its sources, used to restore a conversation after a refresh. */
export interface AskConversationTurn {
  id: string;
  turn_index: number;
  question: string;
  answer: string;
  finish_reason: "stop" | "error" | "no_evidence";
  sources: AskSourceItem[];
  created_at: string;
}

/** Which of the three Ask-answer lanes (spec §5.2, §7.2) a source belongs to. */
export type AskLayer = "note" | "observed" | "insight";

export interface SearchParams {
  query: string;
  source_type?: SourceType | "all";
  exclude_source_types?: SourceType[];
  limit?: number;
  offset?: number;
  sort?: "relevance" | "recent";
  use_hyde?: boolean;
  use_rerank?: boolean;
  curated?: boolean;
  include_deleted?: boolean;
}

export interface StatsResponse {
  by_source: Partial<Record<SourceType, number>>;
  total: number;
}

/** Matches store.BaselineStats in internal/store/document.go */
export interface BaselineStats {
  documents: {
    by_source: Partial<Record<SourceType, { count: number; p50_bytes: number; p95_bytes: number }>>;
    total: number;
  };
  chunks: {
    total: number;
    by_source: Partial<Record<SourceType, number>>;
  };
  extraction_failures: {
    total: number;
    by_source: Partial<Record<SourceType, number>>;
  };
  collection: {
    last_collected_at: Partial<Record<SourceType, string>>;
  };
}

/** Matches store.RecentItem in internal/store/recent_by_kind.go */
export interface RecentItem {
  id: string;
  title: string;
  occurred_at: string | null;
  collected_at: string;
}

export interface RecentItemsResponse {
  kind: string;
  count: number;
  items: RecentItem[];
}

export interface SourcesResponse {
  sources: Partial<Record<SourceType, number>>;
}

export interface DocumentsResponse {
  documents: DocumentDetail[];
}

// ── Knowledge graph (Part B) ─────────────────────────────────────────────
//
// Mirrors graph.EntryNode / Neighbor / EvidenceRef / EntityHit in
// internal/graph/queries.go. The backend normalises nil slices to [], so no
// response field is ever null except EvidenceRef.occurred_at.

/** Entity types projected as secondary Neo4j labels (internal/model/entity.go). */
export const GRAPH_ENTITY_TYPES = ["PERSON", "ORG", "CONCEPT", "OTHER"] as const;
export type GraphEntityType = (typeof GRAPH_ENTITY_TYPES)[number];

/** Closed relation vocabulary (internal/model/relation.go). Values outside
 * this set are downgraded to `related_to` by the backend. */
export const GRAPH_REL_TYPES = [
  "communicated_with",
  "requested_of",
  "committed_to",
  "mentions",
  "belongs_to",
  "scheduled_with",
  "about_topic",
  "related_to",
] as const;
export type GraphRelType = (typeof GRAPH_REL_TYPES)[number];

export interface GraphNode {
  entity_id: number;
  name: string;
  type: string;
  degree: number;
}

export interface GraphNeighbor {
  entity_id: number;
  name: string;
  type: string;
  rel_type: string;
  direction: "out" | "in";
  weight: number;
  last_seen: string;
}

export interface GraphEvidence {
  document_id: string;
  source_type: string;
  occurred_at: string | null;
  confidence: number;
  observed_at: string;
}

export interface GraphEntityHit {
  entity_id: number;
  name: string;
  type: string;
}

/** Shared filter state for the /graph screen. `minConfidence` defaults to 0.5
 * (plan §13); `days` defaults to 30, matching the backend default. */
export interface GraphFilters {
  days: number;
  minConfidence: number;
  entityTypes: string[];
  relTypes: string[];
}

export interface GraphEntryResponse {
  nodes: GraphNode[];
}

export interface GraphExpandResponse {
  neighbors: GraphNeighbor[];
}

export interface GraphEvidenceResponse {
  evidence: GraphEvidence[];
}

export interface GraphEntitiesResponse {
  entities: GraphEntityHit[];
}
