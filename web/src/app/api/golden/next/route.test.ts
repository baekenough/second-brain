import { describe, expect, it } from "vitest";
import { normalizeGoldenNext } from "./route";

describe("normalizeGoldenNext", () => {
  it("빈 문자열 retention을 null로 정규화한다", () => {
    const input = JSON.stringify({
      candidates: [{ document_id: "a", retention: "", stream: "relevance" }],
    });
    expect(JSON.parse(normalizeGoldenNext(input))).toEqual({
      candidates: [{ document_id: "a", retention: null, stream: "relevance" }],
    });
  });

  it("keep/low/disposable/null 값은 그대로 둔다", () => {
    const input = JSON.stringify({
      candidates: [
        { document_id: "a", retention: "keep", stream: "relevance" },
        { document_id: "b", retention: "low", stream: "relevance" },
        { document_id: "c", retention: "disposable", stream: "relevance" },
        { document_id: "d", retention: null, stream: "relevance" },
      ],
    });
    expect(JSON.parse(normalizeGoldenNext(input))).toEqual(JSON.parse(input));
  });

  it("candidates가 배열이 아니면 원문을 그대로 반환한다", () => {
    const input = JSON.stringify({ query: null, candidates: null });
    expect(normalizeGoldenNext(input)).toBe(input);
  });

  it("JSON 파싱에 실패하면 원문을 그대로 반환한다", () => {
    const input = "not json";
    expect(normalizeGoldenNext(input)).toBe(input);
  });

  it("query.window가 없으면 null로 채운다", () => {
    const input = JSON.stringify({
      query: { id: "q1", text: "hi", source: "seed", asked_at: "2026-06-03T00:00:00Z" },
      candidates: [],
    });
    const parsed = JSON.parse(normalizeGoldenNext(input));
    expect(parsed.query.window).toBeNull();
  });

  it("query.window가 이미 있으면 그대로 둔다", () => {
    const window = { from: "2026-05-27T00:00:00Z", to: "2026-06-03T00:00:00Z" };
    const input = JSON.stringify({
      query: { id: "q1", text: "hi", source: "seed", asked_at: "2026-06-03T00:00:00Z", window },
      candidates: [],
    });
    const parsed = JSON.parse(normalizeGoldenNext(input));
    expect(parsed.query.window).toEqual(window);
  });

  it("candidates[].stream이 없거나 알 수 없으면 relevance로 기본값을 채운다", () => {
    const input = JSON.stringify({
      candidates: [
        { document_id: "a", retention: null },
        { document_id: "b", retention: null, stream: "unknown_future_stream" },
        { document_id: "c", retention: null, stream: "recent" },
      ],
    });
    const parsed = JSON.parse(normalizeGoldenNext(input));
    expect(parsed.candidates.map((c: { stream: string }) => c.stream)).toEqual([
      "relevance",
      "relevance",
      "recent",
    ]);
  });
});
