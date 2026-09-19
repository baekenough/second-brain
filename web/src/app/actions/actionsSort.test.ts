import { describe, expect, it } from "vitest";
import { DEFAULT_SORT, parseSort } from "./actionsSort";

describe("parseSort", () => {
  it("recent 값을 그대로 받아들인다", () => {
    expect(parseSort("recent")).toBe("recent");
  });

  it("알 수 없는 값은 기본 정렬(recent)로 되돌린다", () => {
    expect(parseSort("unknown")).toBe(DEFAULT_SORT);
    expect(parseSort(null)).toBe(DEFAULT_SORT);
  });

  it("due는 그대로 유지한다", () => {
    expect(parseSort("due")).toBe("due");
  });
});
