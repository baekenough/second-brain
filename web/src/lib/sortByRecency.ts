/**
 * Defensive client-side "최신순" sort.
 *
 * Every list this project renders is already ordered newest-first by the
 * backend (see `internal/store/document.go`'s `ListRecent`,
 * `internal/store/recent_by_kind.go`, and `internal/store/ask_sessions.go`'s
 * `ListRecentConversations`). This utility is a belt-and-suspenders guard,
 * not the primary sort mechanism: applying it to an already-sorted list is a
 * no-op, and applying it if a backend ordering ever regresses keeps the UI
 * correct without a round-trip fix.
 *
 * Do NOT use this for paginated/offset-based lists — sorting only the current
 * page client-side breaks ordering across page boundaries when the true
 * order must span the full result set (the offset itself must come from a
 * server-side ORDER BY). See web/README or the survey table in PR description
 * for which lists that applies to.
 */

/** Parses an ISO 8601 string into epoch millis. Invalid or missing input
 * sorts as "oldest" (Number.NEGATIVE_INFINITY) rather than throwing, so a
 * single malformed timestamp does not crash the whole list render. */
function toEpochMs(iso: string | null | undefined): number {
  if (!iso) return Number.NEGATIVE_INFINITY;
  const ms = new Date(iso).getTime();
  return Number.isNaN(ms) ? Number.NEGATIVE_INFINITY : ms;
}

/**
 * Returns a new array sorted by `getDate(item)` descending (newest first).
 * Stable: items with an equal or missing date keep their original relative
 * order, which matters because most callers pass an array whose input order
 * is a meaningful secondary tiebreaker (e.g. a fallback field, or the
 * server's own tiebreaker column).
 *
 * Implemented as a decorate-sort-undecorate (Schwartzian transform) so the
 * date is parsed once per item, not once per comparison.
 */
export function sortByRecency<T>(
  items: readonly T[],
  getDate: (item: T) => string | null | undefined,
): T[] {
  return items
    .map((item, index) => ({ item, index, ms: toEpochMs(getDate(item)) }))
    .sort((a, b) => b.ms - a.ms || a.index - b.index)
    .map((decorated) => decorated.item);
}
