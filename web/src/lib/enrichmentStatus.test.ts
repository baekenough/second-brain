import { describe, it, expect } from "vitest";
import { describeEnrichmentStatus } from "./enrichmentStatus";

describe("describeEnrichmentStatus", () => {
  it("maps pending to a non-retryable warning badge", () => {
    const badge = describeEnrichmentStatus("pending");
    expect(badge).toEqual({ label: "정리 중", variant: "warning", retryable: false });
  });

  it("maps done to a non-retryable success badge", () => {
    const badge = describeEnrichmentStatus("done");
    expect(badge).toEqual({ label: "정리 완료", variant: "success", retryable: false });
  });

  it("maps failed to a retryable danger badge", () => {
    const badge = describeEnrichmentStatus("failed");
    expect(badge.variant).toBe("danger");
    expect(badge.retryable).toBe(true);
  });

  it("treats undefined status as pending (a note whose worker hasn't ticked yet)", () => {
    const badge = describeEnrichmentStatus(undefined);
    expect(badge.variant).toBe("warning");
    expect(badge.retryable).toBe(false);
  });
});
