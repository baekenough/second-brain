import type { StatsResponse } from "@/lib/types";

/**
 * Whisper 전사 큐 depth used to be computed client-side as
 * (call-log count) - (call-transcript count). Now the backend reports
 * `transcription_pending` directly on /stats. Older backends predating that
 * field omit it entirely — this is the single place that decides "missing"
 * means 0, so page.tsx and its tests share one interpretation.
 */
export function resolveTranscriptionPending(stats: StatsResponse | null): number {
  return stats?.transcription_pending ?? 0;
}
