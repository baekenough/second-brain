import { afterEach, expect, it, vi } from "vitest";
import {
  generateGoldenQueries,
  getNextGoldenQuery,
  skipGoldenQuery,
  submitGoldenJudgments,
} from "./api";

afterEach(() => vi.unstubAllGlobals());

it("loads an empty queue and advances judgments without creating questions", async () => {
  vi.stubGlobal("window", {});
  const fetchMock = vi
    .fn()
    .mockResolvedValueOnce(
      new Response(
        JSON.stringify({
          query: null,
          candidates: [],
          progress: { open_queries: 0, judged_queries: 0, total_judgments: 0 },
        }),
      ),
    )
    .mockResolvedValueOnce(new Response('{"saved":1,"feedback_applied":1}'))
    .mockResolvedValueOnce(new Response(null, { status: 204 }));
  vi.stubGlobal("fetch", fetchMock);
  expect((await getNextGoldenQuery()).query).toBeNull();
  await submitGoldenJudgments({ query_id: "existing", judgments: [], finish_query: true });
  await skipGoldenQuery("existing");
  expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
    "/api/golden/next?limit=10",
    "/api/golden/judgments",
    "/api/golden/queries/existing/skip",
  ]);
});

it("explicitly generates with POST and preserves zero-created counts", async () => {
  vi.stubGlobal("window", {});
  const fetchMock = vi.fn().mockResolvedValue(new Response('{"created":0,"total_open":2}'));
  vi.stubGlobal("fetch", fetchMock);
  expect(await generateGoldenQueries()).toEqual({ created: 0, total_open: 2 });
  expect(fetchMock).toHaveBeenCalledWith("/api/golden/queries/generate", { method: "POST" });
});

it("does not retry a failed generation request implicitly", async () => {
  vi.stubGlobal("window", {});
  const fetchMock = vi.fn().mockResolvedValue(new Response("", { status: 503 }));
  vi.stubGlobal("fetch", fetchMock);
  await expect(generateGoldenQueries()).rejects.toThrow();
  expect(fetchMock).toHaveBeenCalledTimes(1);
});
