/**
 * Pure selection/keyboard logic for the /golden labeling screen.
 *
 * This lives in its own module (not inside page.tsx) so vitest — which runs
 * with `environment: "node"` — can import it without pulling in next/navigation
 * or any React runtime. Nothing here touches the network or the DOM; the goal
 * is to pin the toggle rule, the payload-building rule, and the keyboard
 * mapping with cheap tests, the same convention app/ask/evidenceFeedback.ts
 * already follows.
 */
import type {
  GoldenCandidate,
  GoldenJudgment,
  GoldenJudgmentInput,
  GoldenQuerySource,
  GoldenQueryWindow,
} from "@/lib/types";

/** Per-candidate selection state, keyed by document_id. A candidate absent
 * from this map has no verdict yet — that is the "선택 안 함" state, distinct
 * from any of the three judgment values. */
export type GoldenSelections = Record<string, GoldenJudgment>;

/**
 * Applies one judgment click. Clicking the judgment a document already has
 * clears the verdict (back to "선택 안 함"); clicking a different judgment
 * overwrites it. This mirrors ask/evidenceFeedback.ts's nextVote toggle rule.
 *
 * Returns a new object — callers own React state and must not mutate the
 * previous selections in place.
 */
export function applyJudgment(
  selections: GoldenSelections,
  documentId: string,
  judgment: GoldenJudgment,
): GoldenSelections {
  const next = { ...selections };
  if (next[documentId] === judgment) {
    delete next[documentId];
  } else {
    next[documentId] = judgment;
  }
  return next;
}

/**
 * Builds the POST /api/golden/judgments payload body from the current
 * selection state. Candidates with no verdict are simply omitted — this is a
 * partial-submit endpoint, so "선택 안 한 카드는 전송하지 않음" falls out of
 * this filter rather than being enforced by the caller.
 *
 * `rank` is taken from each candidate's own `rank` field (its position as the
 * backend returned it), not recomputed from array position, so a caller that
 * re-orders `candidates` for display cannot silently corrupt the rank sent
 * upstream.
 */
export function buildJudgmentInputs(
  candidates: readonly GoldenCandidate[],
  selections: GoldenSelections,
): GoldenJudgmentInput[] {
  const inputs: GoldenJudgmentInput[] = [];
  for (const candidate of candidates) {
    const judgment = selections[candidate.document_id];
    if (judgment !== undefined) {
      inputs.push({ document_id: candidate.document_id, judgment, rank: candidate.rank });
    }
  }
  return inputs;
}

/** Whether every visible candidate has a verdict. Used to decide whether
 * `finish_query` should be sent as true on submit — a partial judgment set
 * still gets `finish_query: false` so the query stays open for the rest. */
export function allJudged(
  candidates: readonly GoldenCandidate[],
  selections: GoldenSelections,
): boolean {
  return candidates.length > 0 && candidates.every((c) => selections[c.document_id] !== undefined);
}

/** Keyboard → judgment mapping for the 1/2/3 shortcuts. Not case-sensitive
 * inputs since digit keys have no case, but kept as a lookup table so the
 * page component never hardcodes the mapping in two places. */
const KEY_JUDGMENTS: Record<string, GoldenJudgment> = {
  "1": "relevant",
  "2": "irrelevant",
  "3": "noise",
};

/** Returns the judgment a keyboard key selects, or undefined if the key is
 * not one of the three judgment shortcuts. */
export function judgmentForKey(key: string): GoldenJudgment | undefined {
  return KEY_JUDGMENTS[key];
}

/** Returns true for the "skip this query" shortcut (S, either case). */
export function isSkipKey(key: string): boolean {
  return key === "s" || key === "S";
}

/** Korean labels for GoldenQuery.source (coordinator confirmation,
 * 2026-09-19). A value outside this map is not an error — the backend may add
 * a new source before this map is updated — so the raw string is shown
 * instead of a generic fallback label. */
const GOLDEN_SOURCE_LABELS: Record<GoldenQuerySource, string> = {
  ask_history: "실제 질문 이력",
  seed: "시드 질의",
  manual: "직접 입력",
  document: "저장 문서 기반",
};

/** Returns the Korean label for a query's source, or the raw value if it is
 * not one of the known sources. */
export function goldenSourceLabel(source: string): string {
  return (GOLDEN_SOURCE_LABELS as Record<string, string>)[source] ?? source;
}

export type FocusDirection = "up" | "down";

/**
 * Moves the focused-card index by one card in the given direction. Clamped at
 * both ends rather than wrapping — with 10 candidates max, wrap-around is
 * more likely to disorient a fast labeler than help them, and clamping keeps
 * the mapping trivial to reason about at the boundaries.
 *
 * Returns 0 for an empty list, and leaves the index untouched (clamped into
 * range) if it was already out of bounds.
 */
export function nextFocusIndex(current: number, direction: FocusDirection, length: number): number {
  if (length <= 0) return 0;
  const clamped = Math.min(Math.max(current, 0), length - 1);
  if (direction === "up") return Math.max(clamped - 1, 0);
  return Math.min(clamped + 1, length - 1);
}

// ── asked_at / window display formatting ────────────────────────────────────
//
// query.asked_at is the backend's REVIEW-TIME reference instant (resolved at
// request time — see internal/api/golden.go), not the moment a historical
// ask_history query was originally asked: a period phrase like "지난주"
// always resolves against what "지난주" means right now, when a judge is
// actually looking at the candidate list. These helpers live here (not in
// lib/dates.ts) because they take an explicit `now` reference instead of
// reading Date.now() themselves, which is what makes them testable without
// faking the system clock.

/** `YYYY-MM-DD`, no time component. Used for both the asked_at label and as
 * a building block for testing — kept separate from lib/dates.ts's
 * locale-formatted variants because the golden screen wants a fixed-width,
 * unambiguous date next to a relative-time parenthetical. */
export function formatAbsoluteDate(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  const year = date.getUTCFullYear();
  const month = String(date.getUTCMonth() + 1).padStart(2, "0");
  const day = String(date.getUTCDate()).padStart(2, "0");
  return `${year}-${month}-${day}`;
}

/** `M/D`, no leading zeros or year — compact enough to fit two of them in a
 * "5/27 ~ 6/3" window label. Uses UTC fields (like formatAbsoluteDate) so the
 * displayed date is stable regardless of the viewer's/runner's local
 * timezone, rather than shifting by a day near midnight. */
export function formatMonthDay(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  return `${date.getUTCMonth() + 1}/${date.getUTCDate()}`;
}

/**
 * "기준 시점" label: `YYYY-MM-DD (N개월 전)`. askedAt is the backend's
 * REVIEW-TIME reference instant (query.asked_at, resolved server-side at
 * request time — see internal/api/golden.go), not the moment the query text
 * was originally asked. The relative part uses the same day-bucket
 * thresholds as lib/dates.ts's formatRelative, but is reimplemented here
 * (rather than imported) so `now` can be injected — formatRelative reads
 * `new Date()` internally, which would make this function's output
 * time-dependent and untestable.
 */
export function formatAskedAtLabel(askedAt: string, now: Date = new Date()): string {
  const date = new Date(askedAt);
  if (Number.isNaN(date.getTime())) return "";
  const diffMs = now.getTime() - date.getTime();
  const diffDays = Math.floor(diffMs / (1000 * 60 * 60 * 24));

  let relative: string;
  if (diffDays <= 0) relative = "오늘";
  else if (diffDays === 1) relative = "어제";
  else if (diffDays < 7) relative = `${diffDays}일 전`;
  else if (diffDays < 30) relative = `${Math.floor(diffDays / 7)}주 전`;
  else if (diffDays < 365) relative = `${Math.floor(diffDays / 30)}개월 전`;
  else relative = `${Math.floor(diffDays / 365)}년 전`;

  return `${formatAbsoluteDate(askedAt)} (${relative})`;
}

/**
 * "검색 창" label. `null` means the query carried no period expression, so
 * the backend searched without a date bound — that state gets an explicit
 * explanation rather than a blank, since a judge seeing no window text at all
 * cannot tell "no window" from "field not loaded yet".
 */
export function formatWindowLabel(window: GoldenQueryWindow | null): string {
  if (!window) return "검색 창: 없음(최근 90일 최신순 섞음)";
  return `검색 창: ${formatMonthDay(window.from)} ~ ${formatMonthDay(window.to)}`;
}

/** Korean labels for GoldenCandidate.stream. */
const GOLDEN_STREAM_LABELS: Record<string, string> = {
  relevance: "관련도",
  recent: "최신",
};

/** Returns the Korean label for a candidate's retrieval stream, or the raw
 * value if it is not one of the two known streams (same fallback convention
 * as goldenSourceLabel). */
export function goldenStreamLabel(stream: string): string {
  return GOLDEN_STREAM_LABELS[stream] ?? stream;
}
