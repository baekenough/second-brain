import { describe, expect, it } from "vitest";
import { buildRankMap, nextVote } from "./evidenceFeedback";
import type { AskSourceItem } from "@/lib/types";

/** Dummy sources only — no real titles, names or numbers (plan §privacy). */
function source(id: string, source_type: AskSourceItem["source_type"]): AskSourceItem {
  return { id, title: "더미 문서", source_type, score: 0.5 };
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
