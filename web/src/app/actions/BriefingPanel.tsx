"use client";

import { useEffect, useMemo, useState, useTransition } from "react";
import Link from "next/link";
import { Badge, Card, Spinner } from "@/components/ui";
import { getBriefing } from "@/lib/api";
import type { BriefingResponse } from "@/lib/types";

export interface BriefingPanelProps {
  /** Bumped by the page after a successful done/ignore. The open action set is
   * the server's cache key, so a refetch after a state change returns freshly
   * generated sentences rather than the cached ones. */
  refreshKey: number;
}

/** Assigns a stable footnote number to each distinct evidence document, in the
 * order the sentences reference them. Numbers are the only link text: a title
 * or a snippet would put document content back into this response, which is
 * exactly what the document_id-only contract avoids (plan Task 8). */
function numberEvidence(data: BriefingResponse): {
  sentences: { text: string; refs: { id: string; n: number }[] }[];
} {
  const numbers = new Map<string, number>();
  const sentences = data.sentences.map((s) => ({
    text: s.text,
    refs: s.document_ids.map((id) => {
      let n = numbers.get(id);
      if (n === undefined) {
        n = numbers.size + 1;
        numbers.set(id, n);
      }
      return { id, n };
    }),
  }));
  return { sentences };
}

export function BriefingPanel({ refreshKey }: BriefingPanelProps) {
  const [data, setData] = useState<BriefingResponse | null>(null);
  // React 19 transition rather than a setState in the effect body (project
  // lint rule react-hooks/set-state-in-effect).
  const [loading, startLoad] = useTransition();

  useEffect(() => {
    let cancelled = false;
    startLoad(async () => {
      try {
        const res = await getBriefing();
        if (!cancelled) setData(res);
      } catch {
        // A failed briefing must not get in the way of the action list, so the
        // panel simply disappears. The error is not logged: the message would
        // carry the request context and the body is personal data.
        if (!cancelled) setData(null);
      }
    });
    return () => {
      cancelled = true;
    };
  }, [refreshKey, startLoad]);

  const numbered = useMemo(() => (data ? numberEvidence(data) : null), [data]);

  if (loading && !data) {
    return (
      <Card padding="md" variant="subtle">
        <div className="flex items-center gap-2 text-sm text-foreground-muted">
          <Spinner size="sm" />
          브리핑 생성 중…
        </div>
      </Card>
    );
  }

  // No data, or nothing to brief on: the panel is not part of the page.
  if (!data || !numbered || data.action_count === 0) return null;

  return (
    <Card padding="md" variant="subtle" aria-busy={loading}>
      <section aria-label="브리핑" className="space-y-2">
        <div className="flex flex-wrap items-center gap-1.5">
          <h2 className="text-xs font-semibold tracking-wide text-foreground-muted uppercase">
            브리핑
          </h2>
          {data.degraded && (
            // The single purpose of this badge is to stop the user reading a
            // deterministic aggregate as a written summary.
            <Badge variant="warning" size="sm">
              요약 생성 실패 — 집계만 표시
            </Badge>
          )}
          {data.cached && (
            <Badge size="sm" title="열린 액션이 바뀌면 자동으로 다시 생성됩니다">
              캐시됨
            </Badge>
          )}
        </div>

        <p className="text-sm leading-relaxed text-foreground">
          {numbered.sentences.map((s, i) => (
            <span key={i}>
              {s.text}
              {s.refs.map((ref) => (
                <sup key={ref.id} className="ml-0.5">
                  <Link
                    href={`/documents/${ref.id}`}
                    aria-label={`근거 문서 ${ref.n}`}
                    className="text-text-accent underline-offset-2 hover:underline"
                  >
                    [{ref.n}]
                  </Link>
                </sup>
              ))}{" "}
            </span>
          ))}
        </p>

        {data.dropped_count > 0 && (
          // A trust signal, not debug output: the user should be able to see
          // that unsupported sentences were thrown away rather than shown.
          <p className="text-xs text-foreground-subtle">
            근거 없는 문장 {data.dropped_count}개 폐기됨 — 이 요약은 완전하지 않을 수 있습니다.
          </p>
        )}
      </section>
    </Card>
  );
}
