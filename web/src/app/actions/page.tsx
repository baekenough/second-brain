"use client";

import { Suspense, useCallback, useEffect, useMemo, useState, useTransition } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { ActionList, ACTION_KIND_LABELS } from "./ActionList";
import { BriefingPanel } from "./BriefingPanel";
import { ActionsDisabledError, listActions, setActionState } from "@/lib/api";
import { ACTION_KINDS, type ActionItem, type ActionKind } from "@/lib/types";
import { Button, Spinner } from "@/components/ui";

type SortMode = "due" | "confidence";
type LoadStatus = "ok" | "disabled" | "error";

const DEFAULT_SORT: SortMode = "due";
/** No confidence floor by default. The production confidence distribution is
 * unknown, and an arbitrary floor would silently hide every LLM-detected
 * action (plan §확정 판단). */
const DEFAULT_MIN_CONFIDENCE = 0;

function parseKinds(raw: string | null): ActionKind[] {
  if (!raw) return [];
  const allowed = new Set<string>(ACTION_KINDS);
  return raw.split(",").filter((k): k is ActionKind => allowed.has(k));
}

function parseSort(raw: string | null): SortMode {
  return raw === "confidence" ? "confidence" : DEFAULT_SORT;
}

function parseConfidence(raw: string | null): number {
  const n = Number(raw);
  if (!Number.isFinite(n) || n < 0 || n > 1) return DEFAULT_MIN_CONFIDENCE;
  return n;
}

function ActionsPageInner() {
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();

  // Filters live in the query string so a view survives a refresh and can be
  // shared — except the counterpart filter, which is a person's name and
  // therefore stays in component state only (plan Task 8: no names in URLs).
  const [kinds, setKinds] = useState<ActionKind[]>(() => parseKinds(searchParams.get("kind")));
  const [sort, setSort] = useState<SortMode>(() => parseSort(searchParams.get("sort")));
  const [minConfidence, setMinConfidence] = useState(() =>
    parseConfidence(searchParams.get("minconf")),
  );
  const [includeArchived, setIncludeArchived] = useState(
    () => searchParams.get("archived") === "true",
  );
  const [counterpartInput, setCounterpartInput] = useState("");
  const [counterpart, setCounterpart] = useState("");

  const [items, setItems] = useState<ActionItem[]>([]);
  const [truncated, setTruncated] = useState(false);
  const [status, setStatus] = useState<LoadStatus>("ok");
  const [loading, startLoad] = useTransition();

  const [failedKeys, setFailedKeys] = useState<ReadonlySet<string>>(() => new Set());
  const [pendingKeys, setPendingKeys] = useState<ReadonlySet<string>>(() => new Set());

  // Bumped after every successful state change: it re-runs the list query and
  // is handed to the briefing panel, whose server-side cache key changes with
  // the open action set.
  const [refreshKey, setRefreshKey] = useState(0);

  // ── URL sync (no names) ───────────────────────────────────────────────
  useEffect(() => {
    const params = new URLSearchParams();
    if (kinds.length > 0) params.set("kind", kinds.join(","));
    if (sort !== DEFAULT_SORT) params.set("sort", sort);
    if (minConfidence !== DEFAULT_MIN_CONFIDENCE) params.set("minconf", String(minConfidence));
    if (includeArchived) params.set("archived", "true");
    const qs = params.toString();
    router.replace(qs ? `${pathname}?${qs}` : pathname, { scroll: false });
  }, [kinds, sort, minConfidence, includeArchived, pathname, router]);

  // ── Counterpart debounce ──────────────────────────────────────────────
  useEffect(() => {
    const timer = setTimeout(() => setCounterpart(counterpartInput.trim()), 300);
    return () => clearTimeout(timer);
  }, [counterpartInput]);

  const params = useMemo(
    () => ({
      kinds,
      sort,
      minConfidence,
      includeArchived,
      counterpart: counterpart || undefined,
    }),
    [kinds, sort, minConfidence, includeArchived, counterpart],
  );

  // ── List query ────────────────────────────────────────────────────────
  useEffect(() => {
    let cancelled = false;
    // A transition rather than a setState in the effect body (project lint
    // rule react-hooks/set-state-in-effect).
    startLoad(async () => {
      try {
        const res = await listActions(params);
        if (cancelled) return;
        setItems(res.actions ?? []);
        setTruncated(res.truncated);
        setStatus("ok");
      } catch (err: unknown) {
        if (cancelled) return;
        setItems([]);
        setTruncated(false);
        // No response body is logged here: summaries and names are personal
        // data, so only the failure mode is distinguished.
        setStatus(err instanceof ActionsDisabledError ? "disabled" : "error");
      }
    });
    return () => {
      cancelled = true;
    };
  }, [params, refreshKey, startLoad]);

  // ── Optimistic done / ignore ──────────────────────────────────────────
  const handleResolve = useCallback(
    (key: string, state: "done" | "ignored") => {
      const index = items.findIndex((i) => i.identity_key === key);
      const snapshot = items[index];
      if (index < 0 || !snapshot) return;

      setItems((prev) => prev.filter((i) => i.identity_key !== key));
      setFailedKeys((prev) => {
        if (!prev.has(key)) return prev;
        const next = new Set(prev);
        next.delete(key);
        return next;
      });
      setPendingKeys((prev) => new Set(prev).add(key));

      setActionState(key, state)
        .then(() => {
          setPendingKeys((prev) => {
            const next = new Set(prev);
            next.delete(key);
            return next;
          });
          // Re-runs the list query and re-generates the briefing.
          setRefreshKey((n) => n + 1);
        })
        .catch(() => {
          // Roll the card back into its old position and flag it. The endpoint
          // is idempotent, so pressing the button again is safe.
          setItems((prev) => {
            if (prev.some((i) => i.identity_key === key)) return prev;
            const next = [...prev];
            next.splice(Math.min(index, next.length), 0, snapshot);
            return next;
          });
          setPendingKeys((prev) => {
            const next = new Set(prev);
            next.delete(key);
            return next;
          });
          setFailedKeys((prev) => new Set(prev).add(key));
        });
    },
    [items],
  );

  const toggleKind = useCallback((kind: ActionKind) => {
    setKinds((prev) => (prev.includes(kind) ? prev.filter((k) => k !== kind) : [...prev, kind]));
  }, []);

  return (
    <main className="mx-auto w-full max-w-3xl space-y-4 px-4 py-6">
      <header className="space-y-1">
        <h1 className="text-xl font-semibold text-foreground">액션</h1>
        <p className="text-sm text-foreground-muted">
          응답이 필요한 대화와 아직 지키지 않은 약속입니다.
        </p>
      </header>

      {status === "ok" && <BriefingPanel refreshKey={refreshKey} />}

      {/* ── Filter bar ─────────────────────────────────────────────────── */}
      <section aria-label="필터" className="space-y-3 rounded-lg border border-border p-3">
        <div className="flex flex-wrap gap-1.5">
          {ACTION_KINDS.map((kind) => {
            const on = kinds.includes(kind);
            return (
              <Button
                key={kind}
                size="sm"
                variant={on ? "primary" : "secondary"}
                aria-pressed={on}
                onClick={() => toggleKind(kind)}
              >
                {ACTION_KIND_LABELS[kind]}
              </Button>
            );
          })}
        </div>

        <div className="flex flex-wrap items-center gap-3">
          <label className="flex items-center gap-2 text-xs text-foreground-muted">
            <span>상대방</span>
            <input
              type="text"
              value={counterpartInput}
              onChange={(e) => setCounterpartInput(e.target.value)}
              placeholder="이름 일부"
              className="h-8 w-40 rounded-md border border-border bg-surface px-2 text-sm text-foreground focus-visible:ring-2 focus-visible:ring-accent/50 focus-visible:outline-none"
            />
          </label>

          <label className="flex items-center gap-2 text-xs text-foreground-muted">
            <span>정렬</span>
            <select
              value={sort}
              onChange={(e) => setSort(e.target.value === "confidence" ? "confidence" : "due")}
              className="h-8 rounded-md border border-border bg-surface px-2 text-sm text-foreground"
            >
              <option value="due">기한순</option>
              <option value="confidence">신뢰도순</option>
            </select>
          </label>

          <label className="flex items-center gap-2 text-xs text-foreground-muted">
            <span>신뢰도 {Math.round(minConfidence * 100)}% 이상</span>
            <input
              type="range"
              min={0}
              max={1}
              step={0.05}
              value={minConfidence}
              onChange={(e) => setMinConfidence(Number(e.target.value))}
              className="w-32"
            />
          </label>

          <label className="flex items-center gap-2 text-xs text-foreground-muted">
            <input
              type="checkbox"
              checked={includeArchived}
              onChange={(e) => setIncludeArchived(e.target.checked)}
            />
            <span>90일 이전 포함</span>
          </label>
        </div>
      </section>

      {/* ── Results ────────────────────────────────────────────────────── */}
      {status === "disabled" && (
        <p className="rounded-lg border border-border p-4 text-sm text-foreground-muted">
          액션 기능이 비활성화되어 있습니다.
        </p>
      )}

      {status === "error" && (
        <p role="alert" className="rounded-lg border border-border p-4 text-sm text-danger">
          액션을 불러오지 못했습니다.
        </p>
      )}

      {status === "ok" && (
        <>
          {loading && items.length === 0 ? (
            <div className="flex items-center gap-2 p-4 text-sm text-foreground-muted">
              <Spinner size="sm" />
              불러오는 중…
            </div>
          ) : items.length === 0 ? (
            <p className="rounded-lg border border-border p-4 text-sm text-foreground-muted">
              열린 액션이 없습니다.
            </p>
          ) : (
            <div aria-busy={loading} className="space-y-3">
              <ActionList
                items={items}
                onResolve={handleResolve}
                failedKeys={failedKeys}
                pendingKeys={pendingKeys}
              />
              {truncated && (
                <p className="text-xs text-foreground-subtle">
                  상위 {items.length}건만 표시됨 — 필터를 좁히세요.
                </p>
              )}
            </div>
          )}
        </>
      )}
    </main>
  );
}

export default function ActionsPage() {
  return (
    <Suspense>
      <ActionsPageInner />
    </Suspense>
  );
}
