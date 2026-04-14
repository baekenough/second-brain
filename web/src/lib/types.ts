export type SourceType = "slack" | "github" | "filesystem";

export type MatchType = "fulltext" | "vector" | "hybrid";

export interface DocumentMetadata {
  source_type: SourceType;
  source_url?: string;
  channel?: string;
  repo?: string;
  folder?: string;
  author?: string;
  path?: string;
  ext?: string;
  [key: string]: string | undefined;
}

export interface SearchResultItem {
  id: string;
  title: string;
  content: string;
  source_type: SourceType;
  match_type: MatchType;
  score: number;
  collected_at: string;
  created_at: string;
  updated_at: string;
  metadata: DocumentMetadata;
}

export interface SearchResponse {
  results: SearchResultItem[];
  total: number;
  query: string;
  took_ms: number;
}

export interface DocumentDetail {
  id: string;
  title: string;
  content: string;
  source_type: SourceType;
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
}

export interface StatsResponse {
  by_source: Record<string, number>;
  total: number;
}
