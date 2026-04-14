"use client";

import { useState, useEffect, Suspense } from "react";
import { useRouter, useSearchParams, usePathname } from "next/navigation";
import { searchDocuments, listRecentDocuments, getStats } from "@/lib/api";
import type { DocumentDetail, SearchResultItem, SourceType, StatsResponse } from "@/lib/types";
import { getExtension, rawUrl } from "@/lib/preview";
import { extractSummary } from "@/lib/summary";
import { formatDateTime } from "@/lib/dates";

type FilterValue = SourceType | "all";

interface FilterOption {
  value: FilterValue;
  label: string;
}

const FILTER_OPTIONS: FilterOption[] = [
  { value: "all", label: "전체" },
  { value: "filesystem", label: "Drive" },
  { value: "slack", label: "Slack" },
  { value: "github", label: "GitHub" },
];

const SOURCE_BADGE_STYLES: Record<SourceType, string> = {
  slack: "bg-purple-100 text-purple-700 dark:bg-purple-900/40 dark:text-purple-300",
  github: "bg-gray-100 text-gray-700 dark:bg-gray-800 dark:text-gray-300",
  filesystem: "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300",
};

const SOURCE_LABELS: Record<SourceType, string> = {
  slack: "Slack",
  github: "GitHub",
  filesystem: "Drive",
};

const MATCH_TYPE_LABELS: Record<string, string> = {
  fulltext: "전문",
  vector: "의미",
  hybrid: "복합",
};

function countForFilter(stats: StatsResponse | null, filter: FilterValue): number | null {
  if (!stats) return null;
  if (filter === "all") {
    return (stats.total ?? 0) - (stats.by_source.slack ?? 0);
  }
  return stats.by_source[filter] ?? 0;
}

function adaptDocument(doc: DocumentDetail): SearchResultItem {
  return {
    id: doc.id,
    title: doc.title,
    content: doc.content,
    source_type: doc.source_type,
    match_type: "fulltext",
    score: 0,
    collected_at: doc.collected_at,
    created_at: doc.created_at,
    updated_at: doc.updated_at,
    metadata: doc.metadata,
  };
}

function SourceBadge({ sourceType }: { sourceType: SourceType }) {
  return (
    <span
      className={`inline-flex items-center px-2 py-0.5 rounded text-xs font-medium ${SOURCE_BADGE_STYLES[sourceType]}`}
    >
      {SOURCE_LABELS[sourceType]}
    </span>
  );
}

function MatchTypeBadge({ matchType }: { matchType: string }) {
  return (
    <span className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-gray-100 text-gray-500 dark:bg-gray-800 dark:text-gray-400">
      {MATCH_TYPE_LABELS[matchType] ?? matchType}
    </span>
  );
}

function ResultCardPreview({ item }: { item: SearchResultItem }) {
  const [imgError, setImgError] = useState(false);
  const ext = getExtension(item.id, item.metadata);
  const isImage = [".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg"].includes(
    ext.toLowerCase()
  );

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

  return (
    <p className="text-sm text-gray-500 dark:text-gray-400 leading-relaxed line-clamp-3">
      {summary}
    </p>
  );
}

function ResultCard({
  item,
  showMatchType = true,
}: {
  item: SearchResultItem;
  showMatchType?: boolean;
}) {
  return (
    <a
      href={`/documents/${item.id}`}
      className="block p-4 border border-gray-200 dark:border-gray-800 rounded-lg hover:border-gray-400 dark:hover:border-gray-600 transition-colors"
    >
      <div className="flex items-start gap-2 mb-2">
        <h2 className="flex-1 text-sm font-medium text-gray-900 dark:text-gray-100 leading-snug">
          {item.title}
        </h2>
        <div className="flex items-center gap-1 shrink-0">
          <SourceBadge sourceType={item.source_type} />
          {showMatchType && <MatchTypeBadge matchType={item.match_type} />}
        </div>
      </div>
      <ResultCardPreview item={item} />
      <div className="mt-2 space-y-0.5 text-xs text-gray-400 dark:text-gray-500">
        {item.created_at && (
          <div>
            <span className="inline-block w-8 text-gray-500 dark:text-gray-500">생성</span>
            <span>{formatDateTime(item.created_at)}</span>
          </div>
        )}
        <div>
          <span className="inline-block w-8 text-gray-500 dark:text-gray-500">수정</span>
          <span>{formatDateTime(item.collected_at)}</span>
        </div>
      </div>
    </a>
  );
}

function parseFilter(raw: string | null): FilterValue {
  const allowed: FilterValue[] = ["all", "filesystem", "slack", "github"];
  if (raw && (allowed as string[]).includes(raw)) return raw as FilterValue;
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
  const [results, setResults] = useState<SearchResultItem[]>([]);
  const [recent, setRecent] = useState<DocumentDetail[]>([]);
  const [total, setTotal] = useState(0);
  const [tookMs, setTookMs] = useState<number | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [stats, setStats] = useState<StatsResponse | null>(null);

  const isSearchMode = submittedQuery !== "";

  // URL sync: state -> searchParams (router.replace, no history entry)
  useEffect(() => {
    const params = new URLSearchParams();
    if (activeFilter !== "all") params.set("filter", activeFilter);
    if (sort !== "relevance") params.set("sort", sort);
    if (submittedQuery) params.set("q", submittedQuery);
    const qs = params.toString();
    const target = qs ? `${pathname}?${qs}` : pathname;
    router.replace(target, { scroll: false });
  }, [activeFilter, sort, submittedQuery, pathname, router]);

  // 소스별 통계 초기 로드 (1회)
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

  // 검색 모드: submittedQuery, activeFilter, sort 변경 시 자동 재검색
  useEffect(() => {
    if (!isSearchMode) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    const isAll = activeFilter === "all";
    searchDocuments({
      query: submittedQuery,
      source_type: isAll ? undefined : activeFilter,
      exclude_source_types: isAll ? ["slack"] : undefined,
      sort,
    })
      .then((r) => {
        if (cancelled) return;
        setResults(r.results ?? []);
        setTotal(r.total ?? 0);
        setTookMs(r.took_ms ?? null);
      })
      .catch((e) => {
        if (cancelled) return;
        setError(e instanceof Error ? e.message : "검색 중 오류가 발생했습니다.");
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [submittedQuery, activeFilter, sort, isSearchMode]);

  // 최근 문서 모드: 검색 전 상태, activeFilter 변경 시 자동 갱신
  useEffect(() => {
    if (isSearchMode) return;
    let cancelled = false;
    const isAll = activeFilter === "all";
    listRecentDocuments(
      10,
      isAll ? undefined : activeFilter,
      isAll ? ["slack"] : undefined,
    )
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

  function handleSearch(e: React.FormEvent) {
    e.preventDefault();
    const trimmed = query.trim();
    if (!trimmed) return;
    setSubmittedQuery(trimmed);
  }

  return (
    <div className="space-y-6">
      <form onSubmit={handleSearch} className="space-y-3">
        <div className="flex gap-2">
          <input
            type="text"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="검색"
            className="flex-1 px-3 py-2 text-sm border border-gray-300 dark:border-gray-700 rounded-lg bg-white dark:bg-gray-900 text-gray-900 dark:text-gray-100 placeholder-gray-400 focus:outline-none focus:ring-2 focus:ring-gray-400 dark:focus:ring-gray-600"
            disabled={loading}
          />
          <button
            type="submit"
            disabled={loading || !query.trim()}
            className="px-4 py-2 text-sm font-medium bg-gray-900 text-white dark:bg-white dark:text-gray-900 rounded-lg disabled:opacity-40 hover:opacity-80 transition-opacity"
          >
            {loading ? "검색 중…" : "검색"}
          </button>
        </div>

        <div className="flex items-center justify-between gap-2 flex-wrap">
          <div className="flex gap-1.5 flex-wrap">
            {FILTER_OPTIONS.map((option) => {
              const count = countForFilter(stats, option.value);
              return (
                <button
                  key={option.value}
                  type="button"
                  onClick={() => setActiveFilter(option.value)}
                  className={`inline-flex items-center gap-1 px-3 py-1 text-xs rounded-full border transition-colors ${
                    activeFilter === option.value
                      ? "border-gray-900 bg-gray-900 text-white dark:border-white dark:bg-white dark:text-gray-900"
                      : "border-gray-300 text-gray-600 dark:border-gray-700 dark:text-gray-400 hover:border-gray-500 dark:hover:border-gray-500"
                  }`}
                >
                  {option.label}
                  {count !== null && (
                    <span
                      className={`text-[10px] tabular-nums leading-none ${
                        activeFilter === option.value
                          ? "opacity-60"
                          : "opacity-50"
                      }`}
                    >
                      {count.toLocaleString()}
                    </span>
                  )}
                </button>
              );
            })}
          </div>
          <select
            value={sort}
            onChange={(e) => setSort(e.target.value as "relevance" | "recent")}
            disabled={loading}
            aria-label="정렬 기준"
            className="px-3 py-1 text-xs rounded-full border border-gray-300 dark:border-gray-700 bg-white dark:bg-gray-900 text-gray-700 dark:text-gray-300 focus:outline-none focus:ring-1 focus:ring-gray-400 dark:focus:ring-gray-600 disabled:opacity-40 transition-opacity"
          >
            <option value="relevance">관련도 순</option>
            <option value="recent">최신순</option>
          </select>
        </div>
      </form>

      {error && (
        <p className="text-sm text-red-500 dark:text-red-400">{error}</p>
      )}

      {isSearchMode && !loading && !error && results.length === 0 && (
        <p className="text-sm text-gray-400 dark:text-gray-500 text-center py-12">
          결과가 없습니다
        </p>
      )}

      {isSearchMode && results.length > 0 && (
        <div className="space-y-3">
          <p className="text-xs text-gray-400 dark:text-gray-500">
            {total}건
            {tookMs !== null && ` · ${tookMs}ms`}
          </p>
          {results.map((item) => (
            <ResultCard key={item.id} item={item} showMatchType />
          ))}
        </div>
      )}

      {!isSearchMode && !loading && !error && (
        <div className="space-y-3">
          {recent.length > 0 && (
            <p className="text-xs text-gray-400 dark:text-gray-500">
              {stats && countForFilter(stats, activeFilter) !== null
                ? activeFilter === "all"
                  ? `전체 ${countForFilter(stats, "all")?.toLocaleString()}건 중 최근 ${recent.length}건`
                  : `${SOURCE_LABELS[activeFilter as SourceType] ?? activeFilter} ${countForFilter(stats, activeFilter)?.toLocaleString()}건 중 최근 ${recent.length}건`
                : "최근 추가된 문서"}
            </p>
          )}
          {recent.length === 0 && (
            <p className="text-sm text-gray-400 dark:text-gray-500 text-center py-12">
              {activeFilter === "all" ? "검색어를 입력하세요" : "해당 소스의 문서가 없습니다"}
            </p>
          )}
          {recent.map((doc) => (
            <ResultCard
              key={doc.id}
              item={adaptDocument(doc)}
              showMatchType={false}
            />
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
