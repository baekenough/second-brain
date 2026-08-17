export type EnrichmentStatus = "pending" | "done" | "failed";

export interface EnrichmentBadge {
  label: string;
  variant: "success" | "warning" | "danger";
  retryable: boolean;
}

const STATUS_MAP: Record<EnrichmentStatus, EnrichmentBadge> = {
  pending: { label: "정리 중", variant: "warning", retryable: false },
  done: { label: "정리 완료", variant: "success", retryable: false },
  // Only the terminal failed state (3 attempts exhausted, spec §6.3) is
  // retryable — a note mid-retry is still "pending", not "failed".
  failed: { label: "정리 실패", variant: "danger", retryable: true },
};

/** A note with no enrichment_status yet (worker hasn't ticked, spec §6.1's
 * 202 response predates the first worker pass) reads the same as pending. */
export function describeEnrichmentStatus(status: EnrichmentStatus | undefined): EnrichmentBadge {
  return STATUS_MAP[status ?? "pending"];
}
