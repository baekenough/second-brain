/**
 * Source types mirror model.SourceType in the Go backend (internal/model/document.go).
 * Keep in sync when new source types are added.
 *
 * `call-log` and `call-transcript` are being unified into a single `call`
 * source type on the backend (see metadata.legacy_source_type /
 * metadata.transcription below). `call-log` and `call-transcript` are kept
 * here as a read compatibility shim for documents collected before the
 * migration ran — do not use them for new UI decisions once the backend
 * cutover ships; prefer `call` + `metadata.transcription`.
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
  | "call"
  | "call-log"
  | "call-transcript"
  | "upload"
  | "note"
  | "insight";

/** Transcription status for a unified `call` document (metadata.transcription).
 * `none`: no transcript exists, content is a one-line call summary.
 * `pending`: a transcript is expected but not yet ready.
 * `done`: content is the full transcript. */
export type CallTranscriptionStatus = "none" | "pending" | "done";

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
  /** Transcription status for a unified `call` document. Absent on
   * non-call documents and on call documents predating the merge. */
  transcription?: CallTranscriptionStatus;
  /** The pre-merge source_type ("call-log" | "call-transcript") this
   * document originated from, kept for audit/debugging. */
  legacy_source_type?: string;
  /** Set when this document was superseded by the call unification merge
   * (e.g. the call-log side once its transcript arrived) — the UI should
   * treat it as hidden/merged rather than a standalone result. */
  merged_into?: string;
  /** document_id of the matching transcript-side document, when this
   * document is the call-log side of a not-yet-merged pair. */
  transcript_source_id?: string;
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

/** One entry of the `sources` SSE event (spec §5.1).
 *
 * `occurred_at` mirrors internal/api/ask.go's `AskSourceItem.OccurredAt
 * *time.Time` with no `omitempty` (issue #218) — the key is ALWAYS present
 * in the wire payload, carrying either an RFC3339 string or `null`. It is
 * intentionally `string | null` here, not `string | undefined` / optional:
 * an optional field would let a backend regression that drops the key pass
 * type-checking silently, defeating the point of fixing the key-vs-value
 * ambiguity that caused #218 in the first place. See SourceCard's use of
 * this field in app/ask/page.tsx for why `null` and "not present" are not
 * the same thing on the wire, even though the UI renders them identically. */
export interface AskSourceItem {
  id: string;
  title: string;
  source_type: SourceType;
  score: number;
  occurred_at: string | null;
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

/** A thumbs judgement on one evidence card: +1, -1, or 0 ("no opinion").
 * Mirrors store.EvidenceVote.Thumbs — the CHECK constraint on feedback.thumbs
 * allows exactly these three values. */
export type EvidenceVote = -1 | 0 | 1;

/** Body of POST /api/feedback/evidence (BFF) → POST /api/v1/feedback/evidence
 * (internal/api/feedback_evidence.go's EvidenceFeedbackRequest).
 *
 * `query` travels in the POST body, never in a query string: it is the user's
 * own words and would otherwise end up in access logs (see issue #208 for the
 * same mistake made with counterpart names on /actions). */
export interface EvidenceFeedbackRequest {
  conversation_id: string;
  query: string;
  document_id: string;
  thumbs: EvidenceVote;
  /** 0-based position of the card in the answer, before layer grouping. */
  rank: number;
  layer: AskLayer;
}

/** Response of POST /api/v1/feedback/evidence — the value the *server* settled
 * on, which overrides the client's optimistic guess. */
export interface EvidenceFeedbackResponse {
  thumbs: EvidenceVote;
}

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
  /** Count of `call` documents with metadata.transcription === "pending".
   * Optional on the wire — older backends omit it; treat missing as 0. */
  transcription_pending?: number;
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

// ── Actions & briefing (Part C) ───────────────────────────────────────────
//
// The three closed vocabularies below mirror internal/model/action.go. They
// are written as const tuples so the filter bar can iterate them without a
// second, drifting list of strings.

export const ACTION_KINDS = [
  "awaiting_my_reply",
  "my_commitment",
  "their_commitment",
  "scheduled",
] as const;
export type ActionKind = (typeof ACTION_KINDS)[number];

export const ACTION_DETECTED_BY = ["structural", "llm", "both"] as const;
export type ActionDetectedBy = (typeof ACTION_DETECTED_BY)[number];

export type ActionState = "open" | "done" | "ignored";

/** One open action. Carries no document body — only a document_id, so the
 * amount of personal data sitting in any proxy or cache along this path stays
 * at a one-line summary (plan Task 8). */
export interface ActionItem {
  identity_key: string;
  document_id: string;
  thread_key: string;
  kind: ActionKind;
  summary: string;
  counterpart_name: string;
  due_at: string | null;
  detected_by: ActionDetectedBy;
  confidence: number;
  observed_at: string;
  state: ActionState;
}

export interface ActionsResponse {
  actions: ActionItem[];
  count: number;
  /** True when the page came back exactly full — there may be more rows
   * behind it. Without this a client cannot tell "50 actions" from "at least
   * 50 actions". */
  truncated: boolean;
}

/** Filter arguments for listActions. `counterpart` is a person's name and is
 * therefore never written into the page URL (see /actions page). */
export interface ActionListParams {
  kinds?: ActionKind[];
  counterpart?: string;
  dueBefore?: string;
  minConfidence?: number;
  sort?: "recent" | "due" | "confidence";
  limit?: number;
  includeArchived?: boolean;
}

/** One briefing sentence with the evidence that justifies it. document_ids is
 * never empty: a sentence the server could not tie to a real document is
 * discarded before it reaches here (spec §7.3). */
export interface BriefingSentence {
  text: string;
  document_ids: string[];
  identity_keys: string[];
}

export interface BriefingResponse {
  sentences: BriefingSentence[];
  /** True when every model sentence failed validation and the response fell
   * back to a deterministic aggregate. */
  degraded: boolean;
  /** How many model sentences were thrown away for lacking evidence. Shown to
   * the user as a trust signal, not as debug output. */
  dropped_count: number;
  action_count: number;
  cached: boolean;
  generated_at: string;
}

// ── Golden set labeling ────────────────────────────────────────────────────
//
// Search-quality golden-set screen: the user judges each candidate document
// returned for a query as relevant / irrelevant / noise. A judgment is both
// an eval ground-truth label and a retention-noise feedback signal (spec:
// GET /api/v1/golden/next, POST /api/v1/golden/judgments).

/** Closed judgment vocabulary — mirrors the backend's golden judgment enum. */
export const GOLDEN_JUDGMENTS = ["relevant", "irrelevant", "noise"] as const;
export type GoldenJudgment = (typeof GOLDEN_JUDGMENTS)[number];

/** Retention tag mirrors model.RetentionTag (internal/model/retention.go).
 * `null` covers both "untagged" (most of the corpus predates segmentation)
 * and "tag absent from the wire payload" — the UI renders both identically
 * as "미태그", the same convention AskSourceItem.occurred_at uses for its
 * own null case. */
export type RetentionTag = "keep" | "low" | "disposable" | null;

/** Known values of GoldenQuery.source (coordinator confirmation, 2026-09-19).
 * Kept as a const tuple rather than baked into GoldenQuery.source's type
 * because an unrecognised value must still round-trip through the UI
 * unchanged — see app/golden/goldenJudge.ts's goldenSourceLabel, which falls
 * back to the raw string for anything outside this set. */
export const GOLDEN_QUERY_SOURCES = ["ask_history", "seed", "manual"] as const;
export type GoldenQuerySource = (typeof GOLDEN_QUERY_SOURCES)[number];

/** A query's search-window expression resolved against `asked_at` — the
 * concrete `[from, to]` range the backend actually searched within. `null`
 * means the query carries no period expression, so the backend fell back to
 * a recency-mixed search over the whole corpus. */
export interface GoldenQueryWindow {
  from: string;
  to: string;
}

export interface GoldenQuery {
  id: string;
  text: string;
  source: string;
  /** When this query was asked (RFC3339) — the real timestamp for
   * `ask_history` queries, the generation timestamp for `seed` queries. Judges
   * must weigh candidate relevance against this moment, not "now" (spec:
   * coordinator confirmation, 2026-09-19). */
  asked_at: string;
  window: GoldenQueryWindow | null;
}

/** Which retrieval lane produced a candidate: `relevance` came back for
 * semantic/keyword match, `recent` came back purely for being recent within
 * the window. */
export const GOLDEN_CANDIDATE_STREAMS = ["relevance", "recent"] as const;
export type GoldenCandidateStream = (typeof GOLDEN_CANDIDATE_STREAMS)[number];

export interface GoldenCandidate {
  document_id: string;
  title: string;
  snippet: string;
  source_type: SourceType;
  occurred_at: string | null;
  retention: RetentionTag;
  segment?: string | null;
  rank: number;
  stream: GoldenCandidateStream;
}

export interface GoldenProgress {
  judged_queries: number;
  open_queries: number;
  total_judgments: number;
}

/** `query: null` means there is nothing left to label — the screen offers
 * "후보 질의 생성" instead of a candidate list. */
export interface GoldenNextResponse {
  query: GoldenQuery | null;
  candidates: GoldenCandidate[];
  progress: GoldenProgress;
}

export interface GoldenJudgmentInput {
  document_id: string;
  judgment: GoldenJudgment;
  rank: number;
}

export interface GoldenJudgmentsRequest {
  query_id: string;
  judgments: GoldenJudgmentInput[];
  finish_query: boolean;
}

export interface GoldenJudgmentsResponse {
  saved: number;
  feedback_applied: number;
}

export interface GoldenGenerateResponse {
  created: number;
  total_open: number;
}
