"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import Link from "next/link";
import {
  generateGoldenQueries,
  getNextGoldenQuery,
  skipGoldenQuery,
  submitGoldenJudgments,
} from "@/lib/api";
import { formatRelative } from "@/lib/dates";
import { Badge, Button, Card, Spinner, SourceBadge } from "@/components/ui";
import type { GoldenCandidate, GoldenJudgment, GoldenProgress, GoldenQuery } from "@/lib/types";
import {
  allJudged,
  applyJudgment,
  buildJudgmentInputs,
  formatAskedAtLabel,
  formatMonthDay,
  formatWindowLabel,
  goldenSourceLabel,
  goldenStreamLabel,
  isSkipKey,
  judgmentForKey,
  nextFocusIndex,
  type GoldenSelections,
} from "./goldenJudge";

type LoadStatus = "loading" | "ok" | "empty" | "error";

const JUDGMENT_LABELS: Record<GoldenJudgment, string> = {
  relevant: "관련",
  irrelevant: "무관",
  noise: "잡음",
};

const JUDGMENT_KEYS: Record<GoldenJudgment, string> = {
  relevant: "1",
  irrelevant: "2",
  noise: "3",
};

/** Selected-state classes per judgment — kept separate from Badge's own
 * variant set because these are toggleable buttons, not static labels. */
const JUDGMENT_ACTIVE_CLASS: Record<GoldenJudgment, string> = {
  relevant: "border-success bg-success/15 text-success",
  irrelevant: "border-border bg-surface-subtle text-foreground",
  noise: "border-danger bg-danger/15 text-danger",
};

function RetentionBadge({ retention }: { retention: GoldenCandidate["retention"] }) {
  if (retention === "keep") {
    return (
      <Badge variant="success" size="sm">
        보존
      </Badge>
    );
  }
  if (retention === "low") {
    return (
      <Badge variant="warning" size="sm">
        저가치
      </Badge>
    );
  }
  if (retention === "disposable") {
    return (
      <Badge variant="danger" size="sm">
        폐기 대상
      </Badge>
    );
  }
  return (
    <Badge variant="default" size="sm">
      미태그
    </Badge>
  );
}

function ProgressBar({ progress }: { progress: GoldenProgress }) {
  const total = progress.judged_queries + progress.open_queries;
  const pct = total > 0 ? Math.round((progress.judged_queries / total) * 100) : 0;
  return (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between text-xs text-foreground-muted">
        <span>
          질의 {progress.judged_queries} / {total} 완료 ({pct}%)
        </span>
        <span>판정 {progress.total_judgments}건</span>
      </div>
      <div className="h-1.5 w-full overflow-hidden rounded-full bg-surface-subtle">
        <div
          className="h-full rounded-full bg-accent transition-[width]"
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  );
}

interface CandidateCardProps {
  candidate: GoldenCandidate;
  focused: boolean;
  selected: GoldenJudgment | undefined;
  onSelect: (judgment: GoldenJudgment) => void;
  onFocus: () => void;
}

function CandidateCard({ candidate, focused, selected, onSelect, onFocus }: CandidateCardProps) {
  return (
    <Card
      variant={focused ? "outlined" : "default"}
      padding="md"
      className={focused ? "border-accent" : undefined}
      onMouseEnter={onFocus}
    >
      <div className="space-y-2.5">
        <div className="flex flex-wrap items-center gap-1.5 text-xs text-foreground-subtle">
          <span className="font-mono">#{candidate.rank + 1}</span>
          <SourceBadge sourceType={candidate.source_type} size="sm" />
          <Badge variant={candidate.stream === "recent" ? "default" : "accent"} size="sm">
            {goldenStreamLabel(candidate.stream)}
          </Badge>
          <RetentionBadge retention={candidate.retention} />
          <span>
            {candidate.occurred_at
              ? `${formatRelative(candidate.occurred_at)} (${formatMonthDay(candidate.occurred_at)})`
              : "시각 미상"}
          </span>
        </div>

        <div>
          <p className="text-sm font-semibold text-foreground">
            {candidate.title || "(제목 없음)"}
          </p>
          <p className="mt-1 line-clamp-3 text-sm leading-relaxed text-foreground-muted">
            {candidate.snippet}
          </p>
        </div>

        <div className="flex items-center justify-between gap-2">
          <Link
            href={`/documents/${candidate.document_id}`}
            className="text-text-accent text-xs underline-offset-2 hover:underline"
            target="_blank"
          >
            원문 보기
          </Link>

          {/* Touch target: each button is h-9 (36px) with generous horizontal
              padding and a gap between them, so this row is usable on a
              narrow phone screen without buttons overlapping a tab bar
              (past incident: a save button hidden behind the tab bar). */}
          <div role="group" aria-label="판정 선택" className="flex items-center gap-1.5">
            {(["relevant", "irrelevant", "noise"] as const).map((judgment) => (
              <button
                key={judgment}
                type="button"
                aria-pressed={selected === judgment}
                onClick={() => onSelect(judgment)}
                className={`flex h-9 items-center gap-1 rounded-md border px-2.5 text-xs font-medium transition-colors ${
                  selected === judgment
                    ? JUDGMENT_ACTIVE_CLASS[judgment]
                    : "border-border bg-surface text-foreground-muted hover:bg-surface-subtle"
                }`}
              >
                <span className="font-mono text-[10px] opacity-70">{JUDGMENT_KEYS[judgment]}</span>
                {JUDGMENT_LABELS[judgment]}
              </button>
            ))}
          </div>
        </div>
      </div>
    </Card>
  );
}

export default function GoldenPage() {
  const [status, setStatus] = useState<LoadStatus>("loading");
  const [query, setQuery] = useState<GoldenQuery | null>(null);
  const [candidates, setCandidates] = useState<GoldenCandidate[]>([]);
  const [progress, setProgress] = useState<GoldenProgress | null>(null);
  const [selections, setSelections] = useState<GoldenSelections>({});
  const [focusIndex, setFocusIndex] = useState(0);
  const [submitting, setSubmitting] = useState(false);
  const [generating, setGenerating] = useState(false);
  const generatingRef = useRef(false);
  const [generationMessage, setGenerationMessage] = useState<string | null>(null);
  const [errorMessage, setErrorMessage] = useState<string | null>(null);

  // Read inside the keydown handler without re-subscribing the listener on
  // every keystroke — the handler is registered once on mount.
  const stateRef = useRef({ query, candidates, selections, focusIndex, submitting, status });
  stateRef.current = { query, candidates, selections, focusIndex, submitting, status };

  const loadNext = useCallback(async () => {
    setStatus("loading");
    setErrorMessage(null);
    try {
      const res = await getNextGoldenQuery(10);
      setQuery(res.query);
      setCandidates(res.candidates);
      setProgress(res.progress);
      setSelections({});
      setFocusIndex(0);
      setStatus(res.query ? "ok" : "empty");
    } catch {
      setStatus("error");
      setErrorMessage("질의를 불러오지 못했습니다.");
    }
  }, []);

  useEffect(() => {
    void loadNext();
  }, [loadNext]);

  const handleSelect = useCallback((documentId: string, judgment: GoldenJudgment) => {
    setSelections((prev) => applyJudgment(prev, documentId, judgment));
  }, []);

  const handleSubmit = useCallback(async () => {
    const {
      query: currentQuery,
      candidates: currentCandidates,
      selections: currentSelections,
      submitting: alreadySubmitting,
    } = stateRef.current;
    if (!currentQuery || alreadySubmitting || generatingRef.current) return;
    const inputs = buildJudgmentInputs(currentCandidates, currentSelections);
    if (inputs.length === 0) return;
    setSubmitting(true);
    setErrorMessage(null);
    try {
      await submitGoldenJudgments({
        query_id: currentQuery.id,
        judgments: inputs,
        finish_query: allJudged(currentCandidates, currentSelections),
      });
      await loadNext();
    } catch {
      setErrorMessage("판정을 저장하지 못했습니다. 다시 시도해 주세요.");
    } finally {
      setSubmitting(false);
    }
  }, [loadNext]);

  const handleSkip = useCallback(async () => {
    const { query: currentQuery, submitting: alreadySubmitting } = stateRef.current;
    if (!currentQuery || alreadySubmitting || generatingRef.current) return;
    setSubmitting(true);
    setErrorMessage(null);
    try {
      await skipGoldenQuery(currentQuery.id);
      await loadNext();
    } catch {
      setErrorMessage("건너뛰지 못했습니다. 다시 시도해 주세요.");
    } finally {
      setSubmitting(false);
    }
  }, [loadNext]);

  // Generation is an explicit user action. A synchronous ref also blocks a
  // second click before React has committed the loading/disabled state.
  const handleGenerate = useCallback(async () => {
    if (generatingRef.current || stateRef.current.submitting) return;
    generatingRef.current = true;
    setGenerating(true);
    setErrorMessage(null);
    setGenerationMessage(null);
    try {
      const result = await generateGoldenQueries();
      setGenerationMessage(
        result.created > 0
          ? `질의 ${result.created}개를 생성했습니다.`
          : result.total_open > 0
            ? "남아 있는 질의를 먼저 판정해 주세요."
            : "추가로 만들 수 있는 질의가 없습니다. 새로운 문서나 질문이 쌓이면 다시 생성해 주세요.",
      );
      // Keep the current question and unsaved judgments intact. Newly created
      // questions join the queue and are loaded by the normal next action.
      if (stateRef.current.status === "ok" && stateRef.current.query) {
        setProgress((current) => current && { ...current, open_queries: result.total_open });
      } else {
        await loadNext();
      }
    } catch {
      setErrorMessage("질의를 생성하지 못했습니다. 생성 버튼을 눌러 다시 시도해 주세요.");
    } finally {
      generatingRef.current = false;
      setGenerating(false);
    }
  }, [loadNext]);

  // ── Keyboard shortcuts: 1/2/3 judge, j/k or arrows move focus, Enter
  // submits, S skips. Registered once; reads live state via stateRef so the
  // fast-labeling loop never re-attaches a listener per keystroke. ──────────
  useEffect(() => {
    function onKeyDown(event: KeyboardEvent) {
      if (event.metaKey || event.ctrlKey || event.altKey) return;
      const target = event.target;
      if (
        target instanceof HTMLElement &&
        (target.closest("input, textarea, select, [contenteditable=true]") ||
          (event.key === "Enter" && target.closest("button, a")))
      ) {
        return;
      }

      const { candidates: currentCandidates, focusIndex: currentFocus } = stateRef.current;

      const judgment = judgmentForKey(event.key);
      if (judgment) {
        const focused = currentCandidates[currentFocus];
        if (focused) {
          event.preventDefault();
          handleSelect(focused.document_id, judgment);
        }
        return;
      }

      if (event.key === "ArrowDown" || event.key === "j") {
        event.preventDefault();
        setFocusIndex((i) => nextFocusIndex(i, "down", currentCandidates.length));
        return;
      }
      if (event.key === "ArrowUp" || event.key === "k") {
        event.preventDefault();
        setFocusIndex((i) => nextFocusIndex(i, "up", currentCandidates.length));
        return;
      }
      if (event.key === "Enter") {
        event.preventDefault();
        void handleSubmit();
        return;
      }
      if (isSkipKey(event.key)) {
        event.preventDefault();
        void handleSkip();
      }
    }

    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [handleSelect, handleSubmit, handleSkip]);

  const judgedCount = Object.keys(selections).length;

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="font-serif text-xl font-semibold text-foreground">골든셋 라벨링</h1>
          <p className="mt-1 text-sm text-foreground-muted">
            질의당 후보 문서를 관련(1) / 무관(2) / 잡음(3)으로 판정하세요. 질문한 시점을 기준으로
            관련 여부를 판단해 주세요. Enter로 제출 후 다음 질의로, S로 건너뛰기.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void handleSkip()}
            disabled={submitting || generating || !query}
          >
            건너뛰기 (S)
          </Button>
          <Button
            variant="secondary"
            size="sm"
            loading={generating}
            disabled={submitting || status === "loading"}
            onClick={() => void handleGenerate()}
          >
            생성
          </Button>
        </div>
      </div>

      <p className="text-sm text-foreground-muted">
        생성 버튼을 누르면 질문 이력과 저장 문서를 바탕으로 새 질의를 만듭니다. 기존 판정은 유지됩니다.
      </p>

      {generationMessage && (
        <p role="status" className="text-sm text-foreground-muted">
          {generationMessage}
        </p>
      )}

      {progress && <ProgressBar progress={progress} />}

      {errorMessage && (
        <p
          role="alert"
          className="rounded-lg border border-danger/30 bg-[--status-danger-light] p-3 text-sm text-danger"
        >
          {errorMessage}
        </p>
      )}

      {status === "loading" && (
        <div className="flex items-center justify-center py-16">
          <Spinner size="lg" />
        </div>
      )}

      {status === "empty" && (
        <Card padding="lg" className="text-center">
          <p className="text-sm text-foreground-muted">판정할 질의가 없습니다.</p>
          <p className="mt-2 text-sm text-foreground-muted">
            위의 생성 버튼을 눌러 질의를 추가해 주세요.
          </p>
        </Card>
      )}

      {status === "error" && !query && (
        <Card padding="lg" className="text-center">
          <p className="text-sm text-danger">불러오는 데 실패했습니다.</p>
          <div className="mt-4">
            <Button variant="secondary" onClick={() => void loadNext()}>
              다시 시도
            </Button>
          </div>
        </Card>
      )}

      {status === "ok" && query && (
        <div className="space-y-4">
          <Card variant="subtle" padding="md">
            <p className="text-xs text-foreground-subtle">
              질의 출처 · {goldenSourceLabel(query.source)}
            </p>
            <p className="mt-1 text-base font-semibold text-foreground">{query.text}</p>
            <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-foreground-muted">
              <span>기준 시점: {formatAskedAtLabel(query.asked_at)}</span>
              <span>{formatWindowLabel(query.window)}</span>
            </div>
          </Card>

          <p className="text-xs text-foreground-subtle">
            {judgedCount} / {candidates.length}개 판정됨
          </p>

          <ul className="space-y-3">
            {candidates.map((candidate, index) => (
              <li key={candidate.document_id}>
                <CandidateCard
                  candidate={candidate}
                  focused={index === focusIndex}
                  selected={selections[candidate.document_id]}
                  onSelect={(judgment) => handleSelect(candidate.document_id, judgment)}
                  onFocus={() => setFocusIndex(index)}
                />
              </li>
            ))}
          </ul>

          <div className="flex justify-end">
            <Button
              loading={submitting}
              disabled={generating || judgedCount === 0}
              onClick={() => void handleSubmit()}
            >
              제출하고 다음으로 (Enter)
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}
