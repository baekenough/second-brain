"use client";

import { useState, useEffect, useTransition, Suspense } from "react";
import Link from "next/link";
import { useRouter, useSearchParams, usePathname } from "next/navigation";
import { searchDocuments, listDocuments, getStats } from "@/lib/api";
import type { DocumentDetail, SearchResultItem, StatsResponse } from "@/lib/types";
import { getExtension, rawUrl } from "@/lib/preview";
import { extractSummary } from "@/lib/summary";
import { formatDateTime } from "@/lib/dates";
import { SOURCE_LABELS, SEARCH_FILTER_SOURCES, DEFAULT_EXCLUDED_SOURCES } from "@/lib/constants";
import type { SourceType } from "@/lib/types";
import { Button, SourceBadge } from "@/components/ui";

type FilterValue = SourceType | "all";

const MATCH_TYPE_LABELS: Record<string, string> = {
  fulltext: "전문",
  vector: "의미",
  hybrid: "복합",
};

function countForFilter(stats: StatsResponse | null, filter: FilterValue): number | null {
  if (!stats) return null;
  if (filter === "all") return stats.total;
  return (stats.by_source as Record<string, number>)[filter] ?? 0;
}

function adaptDocument(doc: DocumentDetail): SearchResultItem {
  return {
    id: doc.id,
    title: doc.title,
    content: doc.content,
    source_type: doc.source_type,
    source_id: doc.source_id ?? "",
    match_type: "fulltext",
    score: 0,
    status: doc.status,
    collected_at: doc.collected_at,
    created_at: doc.created_at,
    updated_at: doc.updated_at,
    metadata: doc.metadata,
  };
}

function MatchTypeBadge({ matchType }: { matchType: string }) {
  return (
    <span className="inline-flex items-center rounded bg-surface-subtle px-2 py-0.5 text-xs font-medium text-foreground-subtle">
      {MATCH_TYPE_LABELS[matchType] ?? matchType}
    </span>
  );
}

function ResultCardPreview({ item }: { item: SearchResultItem }) {
  const [imgError, setImgError] = useState(false);
  const ext = getExtension(item.source_id ?? item.id, item.metadata);
  const isImage = [".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg"].includes(ext.toLowerCase());

  if (isImage && !imgError) {
    return (
      <img
        src={rawUrl(item.id)}
        alt={item.title}
        className="max-h-48 rounded object-contain"
        loading="lazy"
        onError={() => setImgError(true)}
      />
    );
  }

  const summary = extractSummary(item.content);
  if (!summary) return null;

  return <p className="line-clamp-3 text-sm leading-relaxed text-foreground-muted">{summary}</p>;
}

function ResultCard({
  item,
  showMatchType = true,
}: {
  item: SearchResultItem;
  showMatchType?: boolean;
}) {
  return (
    <Link
      href={`/documents/${item.id}`}
      className="block rounded-lg border border-border bg-surface p-4 transition-all hover:border-accent/40 hover:shadow-sm"
    >
      <div className="mb-2 flex items-start gap-2">
        <h2 className="flex-1 text-sm leading-snug font-medium text-foreground">{item.title}</h2>
        <div className="flex shrink-0 items-center gap-1.5">
          <SourceBadge sourceType={item.source_type} />
          {showMatchType && <MatchTypeBadge matchType={item.match_type} />}
        </div>
      </div>
      <ResultCardPreview item={item} />
      <div className="mt-2 space-y-0.5 text-xs text-foreground-subtle">
        {item.created_at && (
          <div>
            <span className="mr-1">생성</span>
            <span>{formatDateTime(item.created_at)}</span>
          </div>
        )}
        <div>
          <span className="mr-1">수집</span>
          <span>{formatDateTime(item.collected_at)}</span>
        </div>
      </div>
    </Link>
  );
}

function parseFilter(raw: string | null): FilterValue {
  if (!raw) return "all";
  const allFilters: (SourceType | "all")[] = [...SEARCH_FILTER_SOURCES];
  if ((allFilters as string[]).includes(raw)) return raw as FilterValue;
  return "all";
}

function parseSort(raw: string | null): "relevance" | "recent" {
  return raw === "recent" ? "recent" : "relevance";
}

function SearchPageInner() {
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();

  const initialFilter = parseFilter(searchParams.get("filter"));
  const initialSort = parseSort(searchParams.get("sort"));
  const initialQuery = searchParams.get("q") ?? "";

  const [query, setQuery] = useState(initialQuery);
  const [submittedQuery, setSubmittedQuery] = useState(initialQuery);
  const [activeFilter, setActiveFilter] = useState<FilterValue>(initialFilter);
  const [sort, setSort] = useState<"relevance" | "recent">(initialSort);

  // Advanced options
  const [showAdvanced, setShowAdvanced] = useState(false);
  const [useHyDE, setUseHyDE] = useState(false);
  const [useRerank, setUseRerank] = useState(false);
  const [useCurated, setUseCurated] = useState(false);

  const [results, setResults] = useState<SearchResultItem[]>([]);
  const [recent, setRecent] = useState<DocumentDetail[]>([]);
  const [total, setTotal] = useState(0);
  const [tookMs, setTookMs] = useState<number | null>(null);
  // React 19 useTransition: isPending = loading; avoids synchronous setState in effects
  const [isPending, startTransition] = useTransition();
  const loading = isPending;
  const [error, setError] = useState<string | null>(null);
  const [stats, setStats] = useState<StatsResponse | null>(null);

  const isSearchMode = submittedQuery !== "";

  // URL sync
  useEffect(() => {
    const params = new URLSearchParams();
    if (activeFilter !== "all") params.set("filter", activeFilter);
    if (sort !== "relevance") params.set("sort", sort);
    if (submittedQuery) params.set("q", submittedQuery);
    const qs = params.toString();
    const target = qs ? `${pathname}?${qs}` : pathname;
    router.replace(target, { scroll: false });
  }, [activeFilter, sort, submittedQuery, pathname, router]);

  // Load stats once
  useEffect(() => {
    let cancelled = false;
    getStats()
      .then((s) => {
        if (!cancelled) setStats(s);
      })
      .catch(() => {
        if (!cancelled) setStats(null);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Search mode — React 19 async transition: no synchronous setState in effect body
  useEffect(() => {
    if (!isSearchMode) return;
    let cancelled = false;
    const isAll = activeFilter === "all";
    startTransition(async () => {
      // Error reset inside transition — not at synchronous effect top-level
      setError(null);
      try {
        const r = await searchDocuments({
          query: submittedQuery,
          source_type: isAll ? undefined : activeFilter,
          exclude_source_types: isAll ? DEFAULT_EXCLUDED_SOURCES : undefined,
          sort,
          use_hyde: useHyDE,
          use_rerank: useRerank,
          curated: useCurated,
        });
        if (cancelled) return;
        setResults(r.results ?? []);
        setTotal(r.count ?? 0);
        setTookMs(r.took_ms ?? null);
      } catch (e: unknown) {
        if (cancelled) return;
        setError(e instanceof Error ? e.message : "검색 중 오류가 발생했습니다.");
      }
    });
    return () => {
      cancelled = true;
    };
  }, [submittedQuery, activeFilter, sort, useHyDE, useRerank, useCurated, isSearchMode]);

  // Recent documents mode
  useEffect(() => {
    if (isSearchMode) return;
    let cancelled = false;
    const isAll = activeFilter === "all";
    listDocuments({
      limit: 10,
      source: isAll ? undefined : activeFilter,
      excludeSource: isAll ? DEFAULT_EXCLUDED_SOURCES.join(",") : undefined,
    })
      .then((r) => {
        if (!cancelled) setRecent(r);
      })
      .catch(() => {
        if (!cancelled) setRecent([]);
      });
    return () => {
      cancelled = true;
    };
  }, [activeFilter, isSearchMode]);

  function handleSearch(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const trimmed = query.trim();
    if (!trimmed) return;
    setSubmittedQuery(trimmed);
  }

  return (
    <div className="space-y-6">
      {/* Search form */}
      <form onSubmit={handleSearch} className="space-y-3">
        <div className="flex gap-2">
          <input
            type="text"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="검색…"
            aria-label="검색어"
            className="flex-1 rounded-lg border border-border bg-surface px-3 py-2.5 text-sm text-foreground transition-shadow placeholder:text-foreground-subtle focus:ring-2 focus:ring-accent/40 focus:outline-none disabled:opacity-50"
            disabled={loading}
          />
          <Button
            type="submit"
            variant="primary"
            size="md"
            loading={loading}
            disabled={loading || !query.trim()}
            icon={loading ? undefined : <span aria-hidden="true">⌕</span>}
          >
            {loading ? "검색 중…" : "검색"}
          </Button>
        </div>

        {/* Filter chips + sort */}
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div className="flex flex-wrap gap-1.5">
            {SEARCH_FILTER_SOURCES.map((src) => {
              const count = countForFilter(stats, src);
              return (
                <button
                  key={src}
                  type="button"
                  onClick={() => setActiveFilter(src)}
                  className={`inline-flex items-center gap-1 rounded-full border px-3 py-1 text-xs transition-colors ${
                    activeFilter === src
                      ? "border-accent bg-accent text-white"
                      : "border-border text-foreground-muted hover:border-accent/50 hover:text-foreground"
                  }`}
                >
                  {src === "all" ? "전체" : SOURCE_LABELS[src]}
                  {count !== null && (
                    <span className="leading-none tabular-nums opacity-60">
                      {count.toLocaleString()}
                    </span>
                  )}
                </button>
              );
            })}
          </div>
          <div className="flex items-center gap-2">
            <select
              value={sort}
              onChange={(e) => setSort(e.target.value as "relevance" | "recent")}
              disabled={loading}
              aria-label="정렬 기준"
              className="rounded-full border border-border bg-surface px-3 py-1 text-xs text-foreground-muted transition-opacity focus:ring-1 focus:ring-accent/40 focus:outline-none disabled:opacity-40"
            >
              <option value="relevance">관련도 순</option>
              <option value="recent">최신순</option>
            </select>
            <button
              type="button"
              onClick={() => setShowAdvanced((v) => !v)}
              className="rounded-full border border-border px-3 py-1 text-xs text-foreground-muted transition-colors hover:border-accent/50 hover:text-foreground"
            >
              고급 {showAdvanced ? "▲" : "▼"}
            </button>
          </div>
        </div>

        {/* Advanced options */}
        {showAdvanced && (
          <div className="flex flex-wrap gap-4 rounded-lg border border-border bg-surface-subtle px-4 py-3 text-sm">
            <label className="flex cursor-pointer items-center gap-2">
              <input
                type="checkbox"
                checked={useHyDE}
                onChange={(e) => setUseHyDE(e.target.checked)}
                className="accent-accent"
              />
              <span className="text-foreground-muted">HyDE 쿼리 확장</span>
            </label>
            <label className="flex cursor-pointer items-center gap-2">
              <input
                type="checkbox"
                checked={useRerank}
                onChange={(e) => setUseRerank(e.target.checked)}
                className="accent-accent"
              />
              <span className="text-foreground-muted">Cross-encoder 재순위</span>
            </label>
            <label className="flex cursor-pointer items-center gap-2">
              <input
                type="checkbox"
                checked={useCurated}
                onChange={(e) => setUseCurated(e.target.checked)}
                className="accent-accent"
              />
              <span className="text-foreground-muted">LLM 큐레이션 (kimi-k2.6)</span>
            </label>
          </div>
        )}
      </form>

      {/* Error */}
      {error && <p className="text-sm text-danger">{error}</p>}

      {/* Search results */}
      {isSearchMode && !loading && !error && results.length === 0 && (
        <p className="py-12 text-center text-sm text-foreground-subtle">결과가 없습니다</p>
      )}

      {isSearchMode && results.length > 0 && (
        <div className="space-y-3">
          <p className="text-xs text-foreground-subtle">
            {total}건{tookMs !== null && ` · ${tookMs}ms`}
          </p>
          {results.map((item) => (
            <ResultCard key={item.id} item={item} showMatchType />
          ))}
        </div>
      )}

      {/* Recent documents */}
      {!isSearchMode && !loading && !error && (
        <div className="space-y-3">
          {recent.length > 0 && <p className="text-xs text-foreground-subtle">최근 추가된 문서</p>}
          {recent.length === 0 && (
            <p className="py-12 text-center text-sm text-foreground-subtle">
              {activeFilter === "all" ? "검색어를 입력하세요" : "해당 소스의 문서가 없습니다"}
            </p>
          )}
          {recent.map((doc) => (
            <ResultCard key={doc.id} item={adaptDocument(doc)} showMatchType={false} />
          ))}
        </div>
      )}
    </div>
  );
}

export default function SearchPage() {
  return (
    <Suspense>
      <SearchPageInner />
    </Suspense>
  );
}
