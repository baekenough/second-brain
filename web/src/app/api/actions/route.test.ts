import { afterEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";
import { GET, POST } from "./route";

afterEach(() => vi.unstubAllGlobals());

describe("actions filter transport", () => {
  it("keeps counterpart names in the upstream POST body", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response('{"actions":[]}'));
    vi.stubGlobal("fetch", fetchMock);
    const response = await POST(
      new NextRequest("http://localhost/api/actions", {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body: new URLSearchParams({
          counterpart: "private-name",
          kind: "scheduled",
          sort: "recent",
        }),
      }),
    );
    expect(response.status).toBe(200);
    expect(response.headers.get("Cache-Control")).toBe("no-store");
    const [url, options] = fetchMock.mock.calls[0]!;
    expect(url).toMatch(/\/api\/v1\/actions$/);
    expect(url).not.toContain("private-name");
    expect(options.method).toBe("POST");
    expect(new URLSearchParams(options.body).get("counterpart")).toBe("private-name");
    expect(new URLSearchParams(options.body).get("kind")).toBe("scheduled");
  });

  it("rejects legacy name query parameters without forwarding them", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const response = await GET(
      new NextRequest("http://localhost/api/actions?counterpart=private-name"),
    );
    expect(response.status).toBe(400);
    expect(await response.text()).not.toContain("private-name");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("preserves non-personal GET filters and repeated kinds", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response('{"actions":[]}'));
    vi.stubGlobal("fetch", fetchMock);
    await GET(
      new NextRequest(
        "http://localhost/api/actions?kind=scheduled&kind=my_commitment&unknown=ignored",
      ),
    );
    const upstream = new URL(fetchMock.mock.calls[0]![0]);
    expect(upstream.searchParams.getAll("kind")).toEqual(["scheduled", "my_commitment"]);
    expect(upstream.searchParams.has("unknown")).toBe(false);
  });
});
