import { describe, it, expect } from "vitest";
import { resolveTranscriptionPending } from "./transcriptionPending";
import type { StatsResponse } from "@/lib/types";

// Whisper 전사 큐 used to be computed client-side as
// (call-log count) - (call-transcript count). Now the backend reports
// transcription_pending directly on /stats. Older backends predating that
// field omit it entirely — this must be treated as 0, not as "loading" or
// NaN, so the dashboard doesn't show a misleading queue depth.
describe("resolveTranscriptionPending", () => {
  it("returns the field value when present", () => {
    const stats = { by_source: {}, total: 10, transcription_pending: 3 } as StatsResponse;
    expect(resolveTranscriptionPending(stats)).toBe(3);
  });

  it("returns 0 when the field is present but zero", () => {
    const stats = { by_source: {}, total: 10, transcription_pending: 0 } as StatsResponse;
    expect(resolveTranscriptionPending(stats)).toBe(0);
  });

  it("returns 0 when the field is missing (older backend)", () => {
    const stats = { by_source: {}, total: 10 } as StatsResponse;
    expect(resolveTranscriptionPending(stats)).toBe(0);
  });

  it("returns 0 when stats has not loaded yet", () => {
    expect(resolveTranscriptionPending(null)).toBe(0);
  });
});
