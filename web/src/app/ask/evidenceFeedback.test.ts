import { describe, expect, it } from "vitest";
import { buildRankMap, nextVote, sourceOccurredAtLabel } from "./evidenceFeedback";
import type { AskSourceItem } from "@/lib/types";

/** Dummy sources only — no real titles, names or numbers (plan §privacy). */
function source(
  id: string,
  source_type: AskSourceItem["source_type"],
  occurred_at: string | null = null,
): AskSourceItem {
  return { id, title: "더미 문서", source_type, score: 0.5, occurred_at };
}

describe("buildRankMap", () => {
  it("레이어와 무관하게 원본 배열 순서로 0-based rank를 매긴다", () => {
    const sources = [
      source("a", "note"),
      source("b", "gmail"),
      source("c", "note"),
      source("d", "insight"),
    ];
    expect(buildRankMap(sources)).toEqual({ a: 0, b: 1, c: 2, d: 3 });
  });

  it("중복 문서 ID가 있으면 첫 등장 순위를 유지한다", () => {
    const sources = [source("a", "note"), source("b", "gmail"), source("a", "note")];
    expect(buildRankMap(sources).a).toBe(0);
  });

  it("빈 배열은 빈 맵이다", () => {
    expect(buildRankMap([])).toEqual({});
  });
});

describe("nextVote", () => {
  it("같은 방향 재클릭은 0으로 토글한다", () => {
    expect(nextVote(1, 1)).toBe(0);
    expect(nextVote(-1, -1)).toBe(0);
  });

  it("반대 방향 클릭은 그 값으로 덮어쓴다", () => {
    expect(nextVote(1, -1)).toBe(-1);
    expect(nextVote(-1, 1)).toBe(1);
  });

  it("undefined에서 클릭하면 클릭 값이 된다", () => {
    expect(nextVote(undefined, 1)).toBe(1);
    expect(nextVote(undefined, -1)).toBe(-1);
  });

  it("0에서 클릭하면 클릭 값이 된다", () => {
    expect(nextVote(0, 1)).toBe(1);
    expect(nextVote(0, -1)).toBe(-1);
  });
});

// #218: `occurred_at`은 항상 키가 존재하고 값은 RFC3339 문자열 또는 null이다.
// null은 "문서에 발생 시각이 없음"과 "과거 대화 이력이라 이 필드가 생기기
// 전에 저장됨"을 구분하지 못하지만, 페이로드에 그 둘을 가를 정보가 없으므로
// sourceOccurredAtLabel은 구분을 시도하지 않고 동일하게 렌더링한다.
describe("sourceOccurredAtLabel", () => {
  it("null이면 잘못된 날짜 문자열 없이 대체 텍스트를 반환한다", () => {
    const label = sourceOccurredAtLabel(null);
    expect(label).toBe("발생 시각 미상");
    expect(label).not.toContain("Invalid Date");
    expect(label).not.toContain("NaN");
    expect(label).not.toContain("1970");
  });

  it("유효한 RFC3339 문자열은 연/월/일/시:분을 포함한 형식으로 표시한다", () => {
    const label = sourceOccurredAtLabel("2026-08-14T09:30:00+09:00");
    // lib/dates의 formatDateTime과 동일한 형식(런타임 로컬 타임존 기준) —
    // 절대 시각이 아니라 형식(자릿수/구두점)만 고정한다. 타임존을 고정하지
    // 않았다는 것 자체가 이 테스트가 확인하려는 계약이다: 어떤 타임존에서
    // 실행되든 "Invalid Date"/"NaN"이 아니라 이 자리표시자 형식이어야 한다.
    expect(label).toMatch(/^\d{4}\. \d{2}\. \d{2}\. \d{2}:\d{2}$/);
    expect(label).not.toContain("Invalid Date");
    expect(label).not.toContain("NaN");
  });

  it("빈 문자열도 null과 동일하게 대체 텍스트로 처리한다", () => {
    expect(sourceOccurredAtLabel("")).toBe("발생 시각 미상");
  });
});
