import { describe, it, expect } from "vitest";
import { SOURCE_LABELS, SOURCE_BADGE_CLASSES, DASHBOARD_SOURCES } from "./constants";

// The backend is unifying source_type "call-log" and "call-transcript" into
// a single "call" type. Documents collected before that migration ran keep
// their legacy source_type on the wire, so the UI must still recognize both
// legacy values — mapped identically to the new "call" type — until the
// backfill completes. See CLAUDE.md task notes for the migration contract
// (metadata.legacy_source_type, metadata.transcription).
describe("call source_type unification — label mapping", () => {
  it("maps the unified call type to the 통화 label", () => {
    expect(SOURCE_LABELS["call"]).toBe("통화");
  });

  it("maps both legacy call types to the same 통화 label as call", () => {
    expect(SOURCE_LABELS["call-log"]).toBe("통화");
    expect(SOURCE_LABELS["call-transcript"]).toBe("통화");
  });
});

describe("call source_type unification — badge class mapping", () => {
  it("maps the unified call type and both legacy types to the same badge class", () => {
    expect(SOURCE_BADGE_CLASSES["call"]).toBe("badge-call");
    expect(SOURCE_BADGE_CLASSES["call-log"]).toBe("badge-call");
    expect(SOURCE_BADGE_CLASSES["call-transcript"]).toBe("badge-call");
  });
});

describe("call source_type unification — dashboard sources", () => {
  it("lists the unified call type exactly once, not the legacy pair", () => {
    expect(DASHBOARD_SOURCES).toContain("call");
    expect(DASHBOARD_SOURCES).not.toContain("call-log");
    expect(DASHBOARD_SOURCES).not.toContain("call-transcript");
    expect(DASHBOARD_SOURCES.filter((s) => s === "call")).toHaveLength(1);
  });
});
