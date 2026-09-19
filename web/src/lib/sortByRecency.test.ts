import { describe, it, expect } from "vitest";
import { sortByRecency } from "./sortByRecency";

interface Item {
  id: string;
  at: string | null | undefined;
}

describe("sortByRecency", () => {
  it("orders items newest-first", () => {
    const items: Item[] = [
      { id: "old", at: "2026-01-01T00:00:00Z" },
      { id: "newest", at: "2026-03-01T00:00:00Z" },
      { id: "middle", at: "2026-02-01T00:00:00Z" },
    ];
    expect(sortByRecency(items, (i) => i.at).map((i) => i.id)).toEqual(["newest", "middle", "old"]);
  });

  it("is stable for equal timestamps — keeps original relative order", () => {
    const items: Item[] = [
      { id: "a", at: "2026-01-01T00:00:00Z" },
      { id: "b", at: "2026-01-01T00:00:00Z" },
      { id: "c", at: "2026-01-01T00:00:00Z" },
    ];
    expect(sortByRecency(items, (i) => i.at).map((i) => i.id)).toEqual(["a", "b", "c"]);
  });

  it("sorts missing/null/invalid dates to the end", () => {
    const items: Item[] = [
      { id: "no-date", at: null },
      { id: "has-date", at: "2026-01-01T00:00:00Z" },
      { id: "undefined-date", at: undefined },
      { id: "bad-date", at: "not-a-date" },
    ];
    expect(sortByRecency(items, (i) => i.at).map((i) => i.id)).toEqual([
      "has-date",
      "no-date",
      "undefined-date",
      "bad-date",
    ]);
  });

  it("does not mutate the input array", () => {
    const items: Item[] = [
      { id: "old", at: "2026-01-01T00:00:00Z" },
      { id: "new", at: "2026-02-01T00:00:00Z" },
    ];
    const original = [...items];
    sortByRecency(items, (i) => i.at);
    expect(items).toEqual(original);
  });

  it("returns an empty array for empty input", () => {
    expect(sortByRecency<Item>([], (i) => i.at)).toEqual([]);
  });
});
