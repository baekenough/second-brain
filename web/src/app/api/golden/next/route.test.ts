import { describe, expect, it } from "vitest";
import { normalizeRetention } from "./route";

describe("normalizeRetention", () => {
  it("빈 문자열 retention을 null로 정규화한다", () => {
    const input = JSON.stringify({ candidates: [{ document_id: "a", retention: "" }] });
    expect(JSON.parse(normalizeRetention(input))).toEqual({
      candidates: [{ document_id: "a", retention: null }],
    });
  });

  it("keep/low/disposable/null 값은 그대로 둔다", () => {
    const input = JSON.stringify({
      candidates: [
        { document_id: "a", retention: "keep" },
        { document_id: "b", retention: "low" },
        { document_id: "c", retention: "disposable" },
        { document_id: "d", retention: null },
      ],
    });
    expect(JSON.parse(normalizeRetention(input))).toEqual(JSON.parse(input));
  });

  it("candidates가 배열이 아니면 원문을 그대로 반환한다", () => {
    const input = JSON.stringify({ query: null, candidates: null });
    expect(normalizeRetention(input)).toBe(input);
  });

  it("JSON 파싱에 실패하면 원문을 그대로 반환한다", () => {
    const input = "not json";
    expect(normalizeRetention(input)).toBe(input);
  });
});
