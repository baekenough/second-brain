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
