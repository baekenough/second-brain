"use client";

import { useEffect, useState, useTransition } from "react";
import Link from "next/link";
import { Badge, Button, SourceBadge, Spinner } from "@/components/ui";
import { getGraphEvidence, GraphUnavailableError } from "@/lib/api";
import type { GraphEvidence, SourceType } from "@/lib/types";
import { formatDateTime } from "@/lib/dates";
import { displayName } from "./mask";
import { REL_TYPE_LABELS } from "./labels";

export interface EvidenceTarget {
  from: number;
  to: number;
  relType: string;
  fromName: string;
  toName: string;
}

export interface EvidencePanelProps {
  target: EvidenceTarget;
  maskNames: boolean;
  onClose: () => void;
}

/**
 * Right-hand panel listing the documents behind one relation.
 *
 * Only source type / occurrence time / confidence are shown: Neo4j never
 * stores document titles or bodies (plan §privacy 1), so reading the original
 * is delegated to /documents/{id}, which is already behind Access + Bearer.
 */
export default function EvidencePanel({ target, maskNames, onClose }: EvidencePanelProps) {
  const [items, setItems] = useState<GraphEvidence[] | null>(null);
  // React 19 transition: the project lint rule forbids setState directly in an
  // effect body, so the fetch and its state writes run inside a transition.
  const [loading, startLoad] = useTransition();
  const [error, setError] = useState<string | null>(null);

  const { from, to, relType } = target;

  useEffect(() => {
    let cancelled = false;
    startLoad(async () => {
      setError(null);
      try {
        const rows = await getGraphEvidence(from, to, relType);
        if (!cancelled) setItems(rows);
      } catch (err: unknown) {
        if (cancelled) return;
        setItems([]);
        setError(
          err instanceof GraphUnavailableError
            ? "그래프를 일시적으로 사용할 수 없습니다."
            : "근거를 불러오지 못했습니다.",
        );
      }
    });
    return () => {
      cancelled = true;
    };
  }, [from, to, relType, startLoad]);

  return (
    <aside
      className="rounded-lg border border-border bg-surface p-4"
      aria-label="선택한 관계의 근거 문서"
    >
      <div className="mb-3 flex items-start justify-between gap-2">
        <div className="min-w-0">
          <h2 className="text-sm font-medium text-foreground">근거 문서</h2>
          <p className="mt-1 text-xs break-words text-foreground-muted">
            {displayName(target.fromName, maskNames)}
            <span className="mx-1 text-foreground-subtle">
              → {REL_TYPE_LABELS[relType] ?? relType} →
            </span>
            {displayName(target.toName, maskNames)}
          </p>
        </div>
        <Button variant="ghost" size="sm" onClick={onClose} aria-label="근거 패널 닫기">
          닫기
        </Button>
      </div>

      {loading && (
        <div className="flex items-center gap-2 text-sm text-foreground-muted">
          <Spinner size="sm" /> 불러오는 중…
        </div>
      )}

      {error && !loading && (
        <p className="rounded-md border border-danger/30 bg-[--status-danger-light] p-2 text-sm text-danger">
          {error}
        </p>
      )}

      {!loading && !error && items !== null && items.length === 0 && (
        <p className="text-sm text-foreground-muted">
          표시할 근거 문서가 없습니다. 이 관계의 문서 멘션이 아직 그래프에 투영되지 않았을 수
          있습니다.
        </p>
      )}

      {!loading && items !== null && items.length > 0 && (
        <ul className="space-y-2">
          {items.map((item) => (
            <li key={item.document_id}>
              <Link
                href={`/documents/${item.document_id}`}
                className="block rounded-md border border-border p-2 transition-colors hover:border-accent/40 hover:bg-surface-subtle"
              >
                <div className="flex items-center gap-1.5">
                  <SourceBadge sourceType={item.source_type as SourceType} size="sm" />
                  <Badge size="sm" variant="default">
                    신뢰도 {item.confidence.toFixed(2)}
                  </Badge>
                </div>
                <div className="mt-1 text-xs text-foreground-subtle">
                  {item.occurred_at ? formatDateTime(item.occurred_at) : "발생 시각 미상"}
                </div>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </aside>
  );
}
