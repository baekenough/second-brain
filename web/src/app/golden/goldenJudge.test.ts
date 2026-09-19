import { describe, expect, it } from "vitest";
import {
  allJudged,
  applyJudgment,
  buildJudgmentInputs,
  goldenSourceLabel,
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
  });

  it("알 수 없는 값은 원문을 그대로 노출한다", () => {
    expect(goldenSourceLabel("unknown_future_source")).toBe("unknown_future_source");
  });
});
