"use client";

import Link from "next/link";
import { Badge, Button, Card } from "@/components/ui";
import type { ActionDetectedBy, ActionItem, ActionKind } from "@/lib/types";

/** Korean labels for the closed `kind` vocabulary (internal/model/action.go).
 * Exported because the filter bar on the page renders the same four words. */
export const ACTION_KIND_LABELS: Record<ActionKind, string> = {
  awaiting_my_reply: "응답 대기",
  my_commitment: "내 약속",
  their_commitment: "상대 약속",
  scheduled: "예정",
};

/** `detected_by` is shown verbatim rather than hidden behind a "verified"
 * flag: it is the user's only handle on an action the LLM invented (spec
 * §7.4). */
const DETECTED_BY_LABELS: Record<ActionDetectedBy, string> = {
  structural: "구조 신호",
  llm: "LLM 추출",
  both: "구조+LLM",
};

const KIND_BADGE_VARIANT: Record<ActionKind, "default" | "accent" | "warning"> = {
  awaiting_my_reply: "warning",
  my_commitment: "accent",
  their_commitment: "default",
  scheduled: "default",
};

const MS_PER_DAY = 24 * 60 * 60 * 1000;

/** Renders a due date as a relative phrase. Returns null when there is no due
 * date, which is the common case for `awaiting_my_reply`. */
export function formatDueLabel(dueAt: string | null, now: Date = new Date()): string | null {
  if (!dueAt) return null;
  const due = new Date(dueAt);
  if (Number.isNaN(due.getTime())) return null;
  // Compare calendar days, not instants: "1일 남음" should not flip to
  // "오늘 마감" because of a few hours' difference.
  const startOfDue = Date.UTC(due.getFullYear(), due.getMonth(), due.getDate());
  const startOfNow = Date.UTC(now.getFullYear(), now.getMonth(), now.getDate());
  const days = Math.round((startOfDue - startOfNow) / MS_PER_DAY);
  if (days === 0) return "오늘 마감";
  if (days > 0) return `${days}일 남음`;
  return `${-days}일 지남`;
}

function DueBadge({ dueAt }: { dueAt: string | null }) {
  const label = formatDueLabel(dueAt);
  if (!label) return null;
  const overdue = label.endsWith("지남");
  const today = label === "오늘 마감";
  return (
    <Badge variant={overdue ? "danger" : today ? "warning" : "default"} size="sm">
      {label}
    </Badge>
  );
}

export interface ActionListProps {
  items: ActionItem[];
  onResolve: (key: string, state: "done" | "ignored") => void;
  /** Keys whose last state change failed. The card keeps its place and offers
   * a retry — the endpoint is idempotent, so retrying costs nothing. */
  failedKeys?: ReadonlySet<string>;
  /** Keys with a state change still in flight, used to disable the buttons. */
  pendingKeys?: ReadonlySet<string>;
}

const EMPTY_KEYS: ReadonlySet<string> = new Set();

export function ActionList({
  items,
  onResolve,
  failedKeys = EMPTY_KEYS,
  pendingKeys = EMPTY_KEYS,
}: ActionListProps) {
  return (
    <ul className="space-y-3">
      {items.map((item) => {
        const failed = failedKeys.has(item.identity_key);
        const pending = pendingKeys.has(item.identity_key);
        return (
          <li key={item.identity_key}>
            {/* The identity key lives in a data attribute only — never in the
                page URL and never in a log line (plan Task 8). */}
            <Card padding="none" data-action-key={item.identity_key}>
              {failed && (
                <p
                  role="alert"
                  className="border-b border-border bg-[--status-danger-light] px-4 py-2 text-xs text-danger"
                >
                  처리에 실패했습니다. 다시 시도해 주세요.
                </p>
              )}
              <div className="space-y-3 p-4">
                <div className="flex flex-wrap items-center gap-1.5">
                  <Badge variant={KIND_BADGE_VARIANT[item.kind]} size="sm">
                    {ACTION_KIND_LABELS[item.kind]}
                  </Badge>
                  <DueBadge dueAt={item.due_at} />
                  <Badge size="sm">{DETECTED_BY_LABELS[item.detected_by]}</Badge>
                  <span className="text-xs text-foreground-subtle">
                    신뢰도 {Math.round(item.confidence * 100)}%
                  </span>
                </div>

                <p className="text-sm leading-relaxed font-semibold text-foreground">
                  {item.summary || ACTION_KIND_LABELS[item.kind]}
                </p>

                <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-foreground-muted">
                  {item.counterpart_name && <span>{item.counterpart_name}</span>}
                  <Link
                    href={`/documents/${item.document_id}`}
                    className="text-text-accent underline-offset-2 hover:underline"
                  >
                    근거 문서
                  </Link>
                </div>

                {/* Buttons stay inside the card. A fixed bottom bar would sit
                    under the mobile tab bar — this project has shipped that
                    bug before. */}
                <div className="flex gap-2">
                  <Button
                    size="sm"
                    variant="primary"
                    disabled={pending}
                    onClick={() => onResolve(item.identity_key, "done")}
                  >
                    완료
                  </Button>
                  <Button
                    size="sm"
                    variant="secondary"
                    disabled={pending}
                    onClick={() => onResolve(item.identity_key, "ignored")}
                  >
                    무시
                  </Button>
                </div>
              </div>
            </Card>
          </li>
        );
      })}
    </ul>
  );
}
