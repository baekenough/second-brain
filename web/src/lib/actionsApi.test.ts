import { afterEach, expect, it, vi } from "vitest";
import { listActions } from "./api";

afterEach(() => vi.unstubAllGlobals());

it("sends action names in the browser request body without URL parameters", async () => {
  vi.stubGlobal("window", {});
  const fetchMock = vi
    .fn()
    .mockResolvedValue(new Response('{"actions":[],"count":0,"truncated":false}'));
  vi.stubGlobal("fetch", fetchMock);
  await listActions({ counterpart: "private-name", kinds: ["scheduled", "my_commitment"] });
  const [url, options] = fetchMock.mock.calls[0]!;
  expect(url).toBe("/api/actions");
  expect(options.method).toBe("POST");
  const body = new URLSearchParams(options.body);
  expect(body.get("counterpart")).toBe("private-name");
  expect(body.getAll("kind")).toEqual(["scheduled", "my_commitment"]);
});
