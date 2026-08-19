/**
 * Per-evidence thumbs helpers for the /ask screen (plan Task 4).
 *
 * These live in their own module rather than inside page.tsx so that the unit
 * test can import them: vitest runs with `environment: "node"` and page.tsx is
 * a "use client" module that pulls in next/navigation.
 *
 * Nothing here touches the network or React — the two functions are pure so
 * the toggle rule and the ranking rule can be pinned by cheap tests.
 */
import type { AskSourceItem, EvidenceVote } from "@/lib/types";
// Relative, not "@/lib/dates": this module is imported directly by
// evidenceFeedback.test.ts, and vitest.config.ts does not resolve the "@"
// alias (only tsconfig.json's paths do, which webpack/Next honours but
// vitest does not) — a value import via the alias fails at test time even
// though `tsc --noEmit` and `next build` both resolve it fine. `import type`
// from "@/lib/types" above is unaffected because type-only imports are
// erased before either bundler sees them.
import { formatDateTime } from "../../lib/dates";

/**
 * Whether the thumbs buttons are rendered at all.
 *
 * Default OFF. This gate is separate from the server's
 * FEEDBACK_EVIDENCE_ENABLED so the frontend can ship before the backend flag
 * is turned on without firing requests at a route that answers 404.
 *
 * `process.env.NEXT_PUBLIC_*` is inlined at build time by Next, so this must
 * stay a literal member access and cannot be re-read at runtime.
 */
export const EVIDENCE_FEEDBACK_ENABLED = process.env.NEXT_PUBLIC_FEEDBACK_EVIDENCE_ENABLED === "1";

/**
 * Maps document id → 0-based position in the answer's evidence list.
 *
 * The rank must come from the *unfiltered* turn.sources array. The UI groups
 * cards by layer before rendering, and an index taken after that grouping
 * would describe the group, not the position the user actually saw — which is
 * the only thing that makes position-bias correction possible later.
 *
 * A document that appears twice keeps its first position: the earlier
 * appearance is the one that carried the exposure.
 */
export function buildRankMap(sources: readonly AskSourceItem[]): Record<string, number> {
  const ranks: Record<string, number> = {};
  sources.forEach((source, index) => {
    if (!(source.id in ranks)) {
      ranks[source.id] = index;
    }
  });
  return ranks;
}

/**
 * The value to show immediately after a click, before the server answers.
 *
 * Clicking the direction that is already set clears the vote (0); clicking the
 * other direction overwrites it. This mirrors store.UpsertEvidence, which is
 * the actual authority — the response's `thumbs` overwrites whatever this
 * predicted.
 */
export function nextVote(current: EvidenceVote | undefined, clicked: 1 | -1): EvidenceVote {
  return current === clicked ? 0 : clicked;
}

/**
 * Display label for a source card's `occurred_at` (issue #218).
 *
 * `null` covers two cases the wire payload cannot distinguish: the document
 * genuinely has no recorded occurrence time, and (for a turn restored from
 * conversation history) the row predates this field's existence and was
 * stored in ask_sessions JSONB before it was captured. There is no
 * information in the payload to tell these apart, so this function does not
 * try — both render identically. Same convention as the graph evidence
 * panel's identical field (app/graph/EvidencePanel.tsx).
 *
 * Uses lib/dates's `formatDateTime`, which formats via the runtime's local
 * timezone (not pinned to KST) — the same convention every other timestamp
 * in this app already follows (dashboard, document detail, evidence panel).
 */
export function sourceOccurredAtLabel(occurredAt: string | null): string {
  if (!occurredAt) return "발생 시각 미상";
  return formatDateTime(occurredAt);
}
