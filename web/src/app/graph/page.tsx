"use client";

import {
  Suspense,
  useCallback,
  useEffect,
  useMemo,
  useState,
  useSyncExternalStore,
  useTransition,
} from "react";
import dynamic from "next/dynamic";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { Badge, Button, Card, Spinner } from "@/components/ui";
import {
  expandGraphNode,
  getGraphEntry,
  GraphUnavailableError,
  searchGraphEntities,
} from "@/lib/api";
import {
  GRAPH_ENTITY_TYPES,
  GRAPH_REL_TYPES,
  type GraphEntityHit,
  type GraphFilters,
  type GraphNode,
} from "@/lib/types";
import type { CanvasLink, CanvasNode } from "./GraphCanvas";
import EvidencePanel, { type EvidenceTarget } from "./EvidencePanel";
import { ENTITY_TYPE_LABELS, REL_TYPE_LABELS } from "./labels";
import {
  displayName,
  getMaskServerSnapshot,
  getMaskSnapshot,
  setMask,
  subscribeMask,
} from "./mask";

// The canvas is loaded client-side only: react-force-graph-2d reaches for
// `window`/`canvas` at import time and would break server rendering. The same
// dynamic boundary keeps the library out of the initial bundle.
const GraphCanvas = dynamic(() => import("./GraphCanvas"), {
  ssr: false,
  loading: () => (
    <div className="flex h-[480px] items-center justify-center rounded-lg border border-border bg-surface">
      <Spinner size="lg" />
    </div>
  ),
});

const DAY_OPTIONS = [7, 30, 90] as const;
const DEFAULT_DAYS = 30;
const DEFAULT_MIN_CONFIDENCE = 0.5;
/** Readability ceiling on the client, separate from the server's own clamps. */
const MAX_CANVAS_NODES = 300;

function parseDays(raw: string | null): number {
  const n = Number(raw);
  return DAY_OPTIONS.includes(n as (typeof DAY_OPTIONS)[number]) ? n : DEFAULT_DAYS;
}

function parseConfidence(raw: string | null): number {
  const n = Number(raw);
  if (raw === null || Number.isNaN(n) || n < 0 || n > 1) return DEFAULT_MIN_CONFIDENCE;
  return n;
}

function parseList(raw: string | null, allowed: readonly string[]): string[] {
  if (raw === null) return [...allowed];
  const picked = raw.split(",").filter((v) => allowed.includes(v));
  return picked.length > 0 ? picked : [...allowed];
}

function parseEntry(raw: string | null): number | null {
  const n = Number(raw);
  return raw && Number.isInteger(n) && n > 0 ? n : null;
}

function toggle(list: string[], value: string, allowed: readonly string[]): string[] {
  const next = list.includes(value) ? list.filter((v) => v !== value) : [...list, value];
  // Empty means "no filter" to the backend, which reads as "everything" —
  // keep the UI honest by refusing to reach that state by unchecking.
  return next.length === 0 ? [...allowed] : next;
}

function linkKey(from: number, to: number, relType: string): string {
  return `${from}->${to}:${relType}`;
}

/** Canvas state for one filter generation (see `genKey`). */
interface GraphSession {
  gen: string;
  nodes: Map<number, CanvasNode>;
  links: Map<string, CanvasLink>;
  expanded: Set<number>;
  capReached: boolean;
  evidence: EvidenceTarget | null;
}

// Shared empties so an invalidated session keeps stable identities and does
// not retrigger the force layout on every render.
const EMPTY_NODES: Map<number, CanvasNode> = new Map();
const EMPTY_LINKS: Map<string, CanvasLink> = new Map();
const EMPTY_EXPANDED: Set<number> = new Set();

function emptySession(gen: string): GraphSession {
  return {
    gen,
    nodes: EMPTY_NODES,
    links: EMPTY_LINKS,
    expanded: EMPTY_EXPANDED,
    capReached: false,
    evidence: null,
  };
}

function GraphPageInner() {
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();

  // Filters live in the query string so a view can be refreshed and shared.
  // Only ids and thresholds go in — never names (plan §privacy).
  const [days, setDays] = useState(() => parseDays(searchParams.get("days")));
  const [minConfidence, setMinConfidence] = useState(() =>
    parseConfidence(searchParams.get("minconf")),
  );
  const [entityTypes, setEntityTypes] = useState<string[]>(() =>
    parseList(searchParams.get("types"), GRAPH_ENTITY_TYPES),
  );
  const [relTypes, setRelTypes] = useState<string[]>(() =>
    parseList(searchParams.get("rels"), GRAPH_REL_TYPES),
  );
  const [entryId, setEntryId] = useState<number | null>(() =>
    parseEntry(searchParams.get("entry")),
  );

  const filters: GraphFilters = useMemo(
    () => ({ days, minConfidence, entityTypes, relTypes }),
    [days, minConfidence, entityTypes, relTypes],
  );

  const [entryNodes, setEntryNodes] = useState<GraphNode[]>([]);
  // React 19 transition: the entry-point load must not setState synchronously
  // inside the effect body (project lint rule), so it runs in a transition.
  const [loading, startEntryLoad] = useTransition();
  const [error, setError] = useState<string | null>(null);

  // Everything built on top of the entry points belongs to one filter
  // generation. Changing a filter invalidates it by key instead of an effect
  // that clears state after the fact.
  const genKey = `${entryId ?? "all"}|${days}|${minConfidence}|${entityTypes.join(",")}|${relTypes.join(",")}`;
  const [session, setSession] = useState<GraphSession>(() => emptySession(genKey));
  const active = session.gen === genKey ? session : emptySession(genKey);

  const [expanding, setExpanding] = useState(false);

  const maskNames = useSyncExternalStore(subscribeMask, getMaskSnapshot, getMaskServerSnapshot);

  const [searchTerm, setSearchTerm] = useState("");
  const [searchHits, setSearchHits] = useState<GraphEntityHit[]>([]);

  // ── URL sync ──────────────────────────────────────────────────────────
  useEffect(() => {
    const params = new URLSearchParams();
    if (days !== DEFAULT_DAYS) params.set("days", String(days));
    if (minConfidence !== DEFAULT_MIN_CONFIDENCE) params.set("minconf", String(minConfidence));
    if (entityTypes.length !== GRAPH_ENTITY_TYPES.length)
      params.set("types", entityTypes.join(","));
    if (relTypes.length !== GRAPH_REL_TYPES.length) params.set("rels", relTypes.join(","));
    if (entryId !== null) params.set("entry", String(entryId));
    const qs = params.toString();
    router.replace(qs ? `${pathname}?${qs}` : pathname, { scroll: false });
  }, [days, minConfidence, entityTypes, relTypes, entryId, pathname, router]);

  // ── Entry points ──────────────────────────────────────────────────────
  useEffect(() => {
    let cancelled = false;
    startEntryLoad(async () => {
      setError(null);
      try {
        const rows = await getGraphEntry(filters);
        if (!cancelled) setEntryNodes(rows);
      } catch (err: unknown) {
        if (cancelled) return;
        setEntryNodes([]);
        setError(
          err instanceof GraphUnavailableError
            ? "그래프를 일시적으로 사용할 수 없습니다. 투영 저장소가 내려갔을 수 있습니다."
            : "진입점을 불러오지 못했습니다.",
        );
      }
    });
    return () => {
      cancelled = true;
    };
  }, [filters, startEntryLoad]);

  // The canvas seed is derived, not stored: entry points, or the single
  // pinned entity when one is selected.
  const seedNodes = useMemo<CanvasNode[]>(() => {
    if (entryId !== null) {
      const known = entryNodes.find((n) => n.entity_id === entryId);
      return [
        {
          id: entryId,
          name: known?.name ?? `엔티티 #${entryId}`,
          type: known?.type ?? "OTHER",
          degree: known?.degree ?? 0,
          seed: true,
        },
      ];
    }
    return entryNodes.map((n) => ({
      id: n.entity_id,
      name: n.name,
      type: n.type,
      degree: n.degree,
      seed: true,
    }));
  }, [entryNodes, entryId]);

  const nodeList = useMemo(() => {
    const merged = new Map<number, CanvasNode>();
    for (const n of seedNodes) merged.set(n.id, n);
    for (const [id, n] of active.nodes) if (!merged.has(id)) merged.set(id, n);
    return [...merged.values()];
  }, [seedNodes, active.nodes]);

  const linkList = useMemo(() => [...active.links.values()], [active.links]);

  // ── Entity search (debounced; the term never reaches the page URL) ─────
  useEffect(() => {
    const term = searchTerm.trim();
    const timer = setTimeout(() => {
      if (term.length < 2) {
        setSearchHits([]);
        return;
      }
      searchGraphEntities(term)
        .then(setSearchHits)
        .catch(() => setSearchHits([]));
    }, 300);
    return () => clearTimeout(timer);
  }, [searchTerm]);

  // ── One-hop expansion ─────────────────────────────────────────────────
  const handleNodeClick = useCallback(
    (node: CanvasNode) => {
      if (active.expanded.has(node.id) || expanding) return;
      if (nodeList.length >= MAX_CANVAS_NODES) {
        setSession((prev) => ({
          ...(prev.gen === genKey ? prev : emptySession(genKey)),
          gen: genKey,
          capReached: true,
        }));
        return;
      }
      setExpanding(true);
      expandGraphNode(node.id, filters)
        .then((neighbors) => {
          setSession((prev) => {
            const base = prev.gen === genKey ? prev : emptySession(genKey);
            const nextNodes = new Map(base.nodes);
            const nextLinks = new Map(base.links);
            for (const n of neighbors) {
              const existing = nextNodes.get(n.entity_id);
              nextNodes.set(n.entity_id, {
                id: n.entity_id,
                name: n.name,
                type: n.type,
                degree: existing?.degree ?? 0,
                seed: false,
              });
              const from = n.direction === "out" ? node.id : n.entity_id;
              const to = n.direction === "out" ? n.entity_id : node.id;
              const key = linkKey(from, to, n.rel_type);
              nextLinks.set(key, {
                key,
                source: from,
                target: to,
                relType: n.rel_type,
                weight: n.weight,
              });
            }
            return {
              gen: genKey,
              nodes: nextNodes,
              links: nextLinks,
              expanded: new Set(base.expanded).add(node.id),
              capReached: base.capReached || nextNodes.size + seedNodes.length > MAX_CANVAS_NODES,
              evidence: base.evidence,
            };
          });
        })
        .catch((err: unknown) => {
          setError(
            err instanceof GraphUnavailableError
              ? "그래프를 일시적으로 사용할 수 없습니다."
              : "이웃을 불러오지 못했습니다.",
          );
        })
        .finally(() => setExpanding(false));
    },
    [active.expanded, expanding, filters, genKey, nodeList.length, seedNodes.length],
  );

  const handleLinkClick = useCallback(
    (link: CanvasLink) => {
      // force-graph swaps source/target for node objects once the layout has
      // run, so accept either shape.
      const from = typeof link.source === "object" ? (link.source as CanvasNode).id : link.source;
      const to = typeof link.target === "object" ? (link.target as CanvasNode).id : link.target;
      const nameOf = (id: number) => nodeList.find((n) => n.id === id)?.name ?? `엔티티 #${id}`;
      setSession((prev) => ({
        ...(prev.gen === genKey ? prev : emptySession(genKey)),
        gen: genKey,
        evidence: { from, to, relType: link.relType, fromName: nameOf(from), toName: nameOf(to) },
      }));
    },
    [genKey, nodeList],
  );

  const resetCanvas = useCallback(() => {
    setEntryId(null);
    // "" never matches a generation key, so the session reads as empty even
    // when the filters themselves did not change.
    setSession(emptySession(""));
  }, []);

  return (
    <div className="space-y-6">
      <header className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="font-serif text-2xl font-semibold tracking-tight">지식 그래프</h1>
          <p className="mt-1 text-sm text-foreground-muted">
            진입점을 고르고 노드를 눌러 한 홉씩 넓혀 보세요. 링크를 누르면 근거 문서가 열립니다.
          </p>
        </div>
        <label className="flex cursor-pointer items-center gap-2 text-sm text-foreground-muted">
          <input
            type="checkbox"
            checked={maskNames}
            onChange={(e) => setMask(e.target.checked)}
            className="h-4 w-4 accent-[--color-accent]"
          />
          이름 가리기
        </label>
      </header>

      {/* ── Filters ── */}
      <Card padding="md" className="space-y-4">
        <div className="flex flex-wrap items-center gap-2">
          <span className="w-20 text-xs text-foreground-subtle">기간</span>
          <div className="flex gap-1" role="group" aria-label="기간 필터">
            {DAY_OPTIONS.map((d) => (
              <Button
                key={d}
                size="sm"
                variant={days === d ? "primary" : "secondary"}
                aria-pressed={days === d}
                onClick={() => setDays(d)}
              >
                {d}일
              </Button>
            ))}
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-2">
          <span className="w-20 text-xs text-foreground-subtle">엔티티</span>
          {GRAPH_ENTITY_TYPES.map((t) => {
            const on = entityTypes.includes(t);
            return (
              <button
                key={t}
                type="button"
                aria-pressed={on}
                onClick={() => setEntityTypes((prev) => toggle(prev, t, GRAPH_ENTITY_TYPES))}
                className="rounded focus-visible:ring-2 focus-visible:ring-accent focus-visible:outline-none"
              >
                <Badge variant={on ? "accent" : "default"}>{ENTITY_TYPE_LABELS[t]}</Badge>
              </button>
            );
          })}
        </div>

        <div className="flex flex-wrap items-center gap-2">
          <span className="w-20 text-xs text-foreground-subtle">관계</span>
          {GRAPH_REL_TYPES.map((t) => {
            const on = relTypes.includes(t);
            return (
              <button
                key={t}
                type="button"
                aria-pressed={on}
                onClick={() => setRelTypes((prev) => toggle(prev, t, GRAPH_REL_TYPES))}
                className="rounded focus-visible:ring-2 focus-visible:ring-accent focus-visible:outline-none"
              >
                <Badge variant={on ? "accent" : "default"}>{REL_TYPE_LABELS[t]}</Badge>
              </button>
            );
          })}
        </div>

        <div className="flex flex-wrap items-center gap-3">
          <label htmlFor="minconf" className="w-20 text-xs text-foreground-subtle">
            최소 신뢰도
          </label>
          <input
            id="minconf"
            type="range"
            min={0}
            max={1}
            step={0.05}
            value={minConfidence}
            onChange={(e) => setMinConfidence(Number(e.target.value))}
            className="w-48 accent-[--color-accent]"
          />
          <span className="font-mono text-xs text-foreground-muted">
            {minConfidence.toFixed(2)}
          </span>
        </div>

        <div className="flex flex-wrap items-center gap-3">
          <label htmlFor="entity-search" className="w-20 text-xs text-foreground-subtle">
            엔티티 검색
          </label>
          <input
            id="entity-search"
            type="search"
            value={searchTerm}
            onChange={(e) => setSearchTerm(e.target.value)}
            placeholder="두 글자 이상 입력"
            autoComplete="off"
            className="h-9 w-64 rounded-md border border-border bg-surface px-3 text-sm text-foreground placeholder:text-foreground-subtle focus-visible:ring-2 focus-visible:ring-accent focus-visible:outline-none"
          />
          {entryId !== null && (
            <Button size="sm" variant="ghost" onClick={resetCanvas}>
              진입점 고정 해제
            </Button>
          )}
        </div>

        {searchHits.length > 0 && (
          <ul className="flex flex-wrap gap-2">
            {searchHits.map((hit) => (
              <li key={hit.entity_id}>
                <Button
                  size="sm"
                  variant="secondary"
                  onClick={() => {
                    setEntryNodes((prev) =>
                      prev.some((n) => n.entity_id === hit.entity_id)
                        ? prev
                        : [...prev, { ...hit, degree: 0 }],
                    );
                    setEntryId(hit.entity_id);
                    setSearchTerm("");
                    setSearchHits([]);
                  }}
                >
                  {displayName(hit.name, maskNames)}
                </Button>
              </li>
            ))}
          </ul>
        )}
      </Card>

      {error && (
        <p
          role="alert"
          className="rounded-md border border-danger/30 bg-[--status-danger-light] p-3 text-sm text-danger"
        >
          {error}
        </p>
      )}

      {active.capReached && (
        <div
          role="status"
          className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-border bg-surface-subtle p-3 text-sm text-foreground-muted"
        >
          <span>노드가 너무 많습니다 — 필터를 좁히거나 초기화하세요.</span>
          <Button size="sm" variant="secondary" onClick={resetCanvas}>
            초기화
          </Button>
        </div>
      )}

      {loading && (
        <div className="flex items-center gap-2 text-sm text-foreground-muted">
          <Spinner size="sm" /> 진입점을 불러오는 중…
        </div>
      )}

      {!loading && !error && entryNodes.length === 0 && (
        <p className="rounded-md border border-border bg-surface-subtle p-4 text-sm text-foreground-muted">
          이 조건에 해당하는 엔티티가 없습니다 — 기간을 넓히거나 신뢰도를 낮춰보세요.
        </p>
      )}

      {/* ── Canvas + evidence ── */}
      {nodeList.length > 0 && (
        <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_20rem]">
          <GraphCanvas
            nodes={nodeList}
            links={linkList}
            onNodeClick={handleNodeClick}
            onLinkClick={handleLinkClick}
            maskNames={maskNames}
            selectedId={entryId}
          />
          {active.evidence && (
            <EvidencePanel
              target={active.evidence}
              maskNames={maskNames}
              onClose={() =>
                setSession((prev) => ({
                  ...(prev.gen === genKey ? prev : emptySession(genKey)),
                  gen: genKey,
                  evidence: null,
                }))
              }
            />
          )}
        </div>
      )}

      {/* ── Entry point list (works without the canvas) ── */}
      {entryNodes.length > 0 && (
        <section aria-label="진입점 목록" className="space-y-2">
          <h2 className="text-sm font-medium text-foreground">진입점 {entryNodes.length}개</h2>
          <ul className="grid gap-2 sm:grid-cols-2">
            {entryNodes.map((n) => (
              <li key={n.entity_id}>
                <button
                  type="button"
                  onClick={() => setEntryId(n.entity_id)}
                  aria-pressed={entryId === n.entity_id}
                  className={`flex w-full items-center justify-between gap-2 rounded-lg border p-3 text-left transition-colors hover:bg-surface-subtle ${
                    entryId === n.entity_id ? "border-accent" : "border-border"
                  }`}
                >
                  <span className="min-w-0 truncate text-sm text-foreground">
                    {displayName(n.name, maskNames)}
                  </span>
                  <span className="flex shrink-0 items-center gap-1.5">
                    <Badge size="sm">{ENTITY_TYPE_LABELS[n.type] ?? n.type}</Badge>
                    <span className="font-mono text-xs text-foreground-subtle">{n.degree}</span>
                  </span>
                </button>
              </li>
            ))}
          </ul>
        </section>
      )}
    </div>
  );
}

export default function GraphPage() {
  return (
    <Suspense>
      <GraphPageInner />
    </Suspense>
  );
}
