import { afterEach, expect, it, vi } from "vitest";
import { POST } from "./route";

afterEach(() => vi.unstubAllGlobals());

it("forwards one explicit POST and preserves zero-created results", async () => {
  const fetchMock = vi.fn().mockResolvedValue(new Response('{"created":0,"total_open":2}'));
  vi.stubGlobal("fetch", fetchMock);
  const response = await POST();
  expect(await response.json()).toEqual({ created: 0, total_open: 2 });
  expect(fetchMock).toHaveBeenCalledTimes(1);
  const [url, options] = fetchMock.mock.calls[0]!;
  expect(url).toMatch(/\/api\/v1\/golden\/queries\/generate$/);
  expect(options.method).toBe("POST");
  expect(options.signal).toBeInstanceOf(AbortSignal);
});

it("propagates a generation failure without issuing another request", async () => {
  const fetchMock = vi.fn().mockResolvedValue(new Response('{"error":"failed"}', { status: 503 }));
  vi.stubGlobal("fetch", fetchMock);
  const response = await POST();
  expect(response.status).toBe(503);
  expect(fetchMock).toHaveBeenCalledTimes(1);
});
