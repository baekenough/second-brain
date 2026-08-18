import { describe, expect, it } from "vitest";
import { displayName, maskName } from "./mask";

describe("maskName", () => {
  it("keeps the first character and masks the rest", () => {
    expect(maskName("더미갑")).toBe("더**");
    expect(maskName("Acme Corp")).toBe("A********");
  });

  it("handles short and empty input", () => {
    expect(maskName("A")).toBe("A");
    expect(maskName("")).toBe("");
  });

  it("counts astral-plane characters as one character", () => {
    expect(maskName("😀ab")).toBe("😀**");
  });
});

describe("displayName", () => {
  it("passes the name through when masking is off", () => {
    expect(displayName("더미을", false)).toBe("더미을");
    expect(displayName("더미을", true)).toBe("더**");
  });
});
