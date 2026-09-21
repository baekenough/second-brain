import { describe, expect, it } from "vitest";
import {
  allJudged,
  applyJudgment,
  buildJudgmentInputs,
  formatAbsoluteDate,
  formatAskedAtLabel,
  formatMonthDay,
  formatWindowLabel,
  goldenSourceLabel,
  goldenStreamLabel,
  isSkipKey,
  judgmentForKey,
  nextFocusIndex,
  type GoldenSelections,
} from "./goldenJudge";
import type { GoldenCandidate } from "@/lib/types";

/** Dummy candidates only — no real titles or snippets (project convention:
 * test fixtures never carry real personal search material). */
function candidate(documentId: string, rank: number): GoldenCandidate {
  return {
    document_id: documentId,
    title: "더미 문서",
    snippet: "더미 스니펫",
    source_type: "note",
    occurred_at: null,
    retention: null,
    rank,
    stream: "relevance",
  };
}

describe("applyJudgment", () => {
  it("선택이 없던 문서를 클릭하면 해당 판정이 설정된다", () => {
    const next = applyJudgment({}, "a", "relevant");
    expect(next).toEqual({ a: "relevant" });
  });

  it("같은 판정을 다시 클릭하면 선택이 해제된다", () => {
    const selections: GoldenSelections = { a: "relevant" };
    const next = applyJudgment(selections, "a", "relevant");
    expect(next).toEqual({});
  });

  it("다른 판정을 클릭하면 값이 덮어써진다", () => {
    const selections: GoldenSelections = { a: "relevant" };
    const next = applyJudgment(selections, "a", "noise");
    expect(next).toEqual({ a: "noise" });
  });

  it("원본 객체를 변형하지 않는다", () => {
    const selections: GoldenSelections = { a: "relevant" };
    applyJudgment(selections, "a", "noise");
    expect(selections).toEqual({ a: "relevant" });
  });
});

describe("buildJudgmentInputs", () => {
  it("선택된 문서만 payload에 포함하고 rank는 후보 자신의 rank를 쓴다", () => {
    const candidates = [candidate("a", 0), candidate("b", 1), candidate("c", 2)];
    const selections: GoldenSelections = { a: "relevant", c: "noise" };
    expect(buildJudgmentInputs(candidates, selections)).toEqual([
      { document_id: "a", judgment: "relevant", rank: 0 },
      { document_id: "c", judgment: "noise", rank: 2 },
    ]);
  });

  it("선택이 하나도 없으면 빈 배열이다", () => {
    const candidates = [candidate("a", 0)];
    expect(buildJudgmentInputs(candidates, {})).toEqual([]);
  });

  it("배열 순서가 아니라 후보의 rank 필드를 그대로 전달한다", () => {
    // 표시 순서가 바뀌어도(예: 정렬) rank는 후보 자신의 것을 유지해야 한다.
    const candidates = [candidate("a", 5), candidate("b", 2)];
    const selections: GoldenSelections = { a: "irrelevant", b: "irrelevant" };
    const inputs = buildJudgmentInputs(candidates, selections);
    expect(inputs.find((i) => i.document_id === "a")?.rank).toBe(5);
    expect(inputs.find((i) => i.document_id === "b")?.rank).toBe(2);
  });
});

describe("allJudged", () => {
  it("모든 후보에 판정이 있으면 true다", () => {
    const candidates = [candidate("a", 0), candidate("b", 1)];
    expect(allJudged(candidates, { a: "relevant", b: "noise" })).toBe(true);
  });

  it("일부만 판정되면 false다", () => {
    const candidates = [candidate("a", 0), candidate("b", 1)];
    expect(allJudged(candidates, { a: "relevant" })).toBe(false);
  });

  it("후보가 없으면 false다 (빈 목록을 '전부 완료'로 취급하지 않는다)", () => {
    expect(allJudged([], {})).toBe(false);
  });
});

describe("judgmentForKey", () => {
  it("1/2/3을 관련/무관/잡음으로 매핑한다", () => {
    expect(judgmentForKey("1")).toBe("relevant");
    expect(judgmentForKey("2")).toBe("irrelevant");
    expect(judgmentForKey("3")).toBe("noise");
  });

  it("매핑되지 않은 키는 undefined다", () => {
    expect(judgmentForKey("4")).toBeUndefined();
    expect(judgmentForKey("a")).toBeUndefined();
  });
});

describe("isSkipKey", () => {
  it("대소문자 상관없이 S를 건너뛰기 키로 인식한다", () => {
    expect(isSkipKey("s")).toBe(true);
    expect(isSkipKey("S")).toBe(true);
  });

  it("다른 키는 건너뛰기 키가 아니다", () => {
    expect(isSkipKey("1")).toBe(false);
    expect(isSkipKey("Enter")).toBe(false);
  });
});

describe("nextFocusIndex", () => {
  it("아래로 이동하면 인덱스가 하나 증가한다", () => {
    expect(nextFocusIndex(0, "down", 5)).toBe(1);
  });

  it("위로 이동하면 인덱스가 하나 감소한다", () => {
    expect(nextFocusIndex(2, "up", 5)).toBe(1);
  });

  it("맨 위에서 위로 이동해도 0에서 멈춘다", () => {
    expect(nextFocusIndex(0, "up", 5)).toBe(0);
  });

  it("맨 아래에서 아래로 이동해도 마지막 인덱스에서 멈춘다", () => {
    expect(nextFocusIndex(4, "down", 5)).toBe(4);
  });

  it("목록이 비어 있으면 항상 0이다", () => {
    expect(nextFocusIndex(0, "down", 0)).toBe(0);
    expect(nextFocusIndex(0, "up", 0)).toBe(0);
  });
});

describe("goldenSourceLabel", () => {
  it("알려진 source 값을 한국어 라벨로 매핑한다", () => {
    expect(goldenSourceLabel("ask_history")).toBe("실제 질문 이력");
    expect(goldenSourceLabel("seed")).toBe("시드 질의");
    expect(goldenSourceLabel("manual")).toBe("직접 입력");
    expect(goldenSourceLabel("document")).toBe("저장 문서 기반");
  });

  it("알 수 없는 값은 원문을 그대로 노출한다", () => {
    expect(goldenSourceLabel("unknown_future_source")).toBe("unknown_future_source");
  });
});

describe("goldenStreamLabel", () => {
  it("알려진 stream 값을 한국어 라벨로 매핑한다", () => {
    expect(goldenStreamLabel("relevance")).toBe("관련도");
    expect(goldenStreamLabel("recent")).toBe("최신");
  });

  it("알 수 없는 값은 원문을 그대로 노출한다", () => {
    expect(goldenStreamLabel("unknown_future_stream")).toBe("unknown_future_stream");
  });
});

describe("formatAbsoluteDate", () => {
  it("ISO 문자열을 YYYY-MM-DD로 변환한다", () => {
    expect(formatAbsoluteDate("2026-06-03T04:00:00Z")).toMatch(/^\d{4}-\d{2}-\d{2}$/);
  });

  it("파싱 불가능한 값은 빈 문자열이다", () => {
    expect(formatAbsoluteDate("not-a-date")).toBe("");
  });
});

describe("formatMonthDay", () => {
  it("ISO 문자열을 M/D로 변환한다", () => {
    expect(formatMonthDay("2026-06-03T04:00:00Z")).toMatch(/^\d{1,2}\/\d{1,2}$/);
  });

  it("파싱 불가능한 값은 빈 문자열이다", () => {
    expect(formatMonthDay("not-a-date")).toBe("");
  });
});

describe("formatAskedAtLabel", () => {
  it("주입된 now 기준으로 '개월 전'을 계산한다 (약 3개월 전)", () => {
    const askedAt = "2026-06-03T00:00:00Z";
    const now = new Date("2026-09-03T00:00:00Z");
    expect(formatAskedAtLabel(askedAt, now)).toBe("2026-06-03 (3개월 전)");
  });

  it("같은 날이면 '오늘'이다", () => {
    const askedAt = "2026-06-03T00:00:00Z";
    const now = new Date("2026-06-03T12:00:00Z");
    expect(formatAskedAtLabel(askedAt, now)).toBe("2026-06-03 (오늘)");
  });

  it("1년 이상이면 '년 전'이다", () => {
    const askedAt = "2024-06-03T00:00:00Z";
    const now = new Date("2026-06-03T00:00:00Z");
    expect(formatAskedAtLabel(askedAt, now)).toBe("2024-06-03 (2년 전)");
  });

  it("파싱 불가능한 값은 빈 문자열이다", () => {
    expect(formatAskedAtLabel("not-a-date", new Date())).toBe("");
  });
});

describe("formatWindowLabel", () => {
  it("window가 있으면 M/D ~ M/D 형식으로 표시한다", () => {
    const label = formatWindowLabel({
      from: "2026-05-27T00:00:00Z",
      to: "2026-06-03T00:00:00Z",
    });
    expect(label).toMatch(/^검색 창: \d{1,2}\/\d{1,2} ~ \d{1,2}\/\d{1,2}$/);
  });

  it("window가 null이면 '없음' 설명을 표시한다", () => {
    expect(formatWindowLabel(null)).toBe("검색 창: 없음(최근 90일 최신순 섞음)");
  });
});
