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
  | "upload";

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
