import { describe, it, expect } from "vitest";
import { splitSSEBuffer, parseAskEvent } from "./sseEvents";

const SOURCES_EVENT =
  'event: sources\ndata: {"sources":[{"id":"a","title":"t","source_type":"sms","score":0.8}]}\n\n';
const TOKEN_EVENT = 'event: token\ndata: {"text":"hello "}\n\n';
const DONE_EVENT = 'event: done\ndata: {"finish_reason":"stop"}\n\n';
const ERROR_EVENT = 'event: error\ndata: {"message":"boom"}\n\n';

describe("splitSSEBuffer", () => {
  it("parses a single complete event and leaves no remainder", () => {
    const { events, remainder } = splitSSEBuffer(TOKEN_EVENT);
    expect(events).toHaveLength(1);
    expect(events[0]).toEqual({ event: "token", data: '{"text":"hello "}' });
    expect(remainder).toBe("");
  });

  it("parses multiple events arriving in one chunk", () => {
    const { events } = splitSSEBuffer(SOURCES_EVENT + TOKEN_EVENT + DONE_EVENT);
    expect(events.map((e) => e.event)).toEqual(["sources", "token", "done"]);
  });

  it("holds back an incomplete trailing event across chunk boundaries", () => {
    // Simulates fetch's ReadableStream splitting mid-event — a byte
    // boundary that lands inside the DONE_EVENT, not at its edge.
    const chunk1 = TOKEN_EVENT + DONE_EVENT.slice(0, 10);
    const chunk2 = DONE_EVENT.slice(10);

    const first = splitSSEBuffer(chunk1);
    expect(first.events).toEqual([{ event: "token", data: '{"text":"hello "}' }]);
    expect(first.remainder).toBe(DONE_EVENT.slice(0, 10));

    const second = splitSSEBuffer(first.remainder + chunk2);
    expect(second.events).toEqual([{ event: "done", data: '{"finish_reason":"stop"}' }]);
    expect(second.remainder).toBe("");
  });
});

describe("parseAskEvent", () => {
  it("parses a sources event", () => {
    // Non-null assertion is safe: the "parses a single complete event" test
    // above already establishes that a well-formed event produces exactly
    // one element; noUncheckedIndexedAccess (tsconfig.json) otherwise types
    // array destructuring as possibly-undefined.
    const [raw] = splitSSEBuffer(SOURCES_EVENT).events;
    expect(parseAskEvent(raw!)).toEqual({
      type: "sources",
      sources: [{ id: "a", title: "t", source_type: "sms", score: 0.8 }],
    });
  });

  it("parses a token event", () => {
    const [raw] = splitSSEBuffer(TOKEN_EVENT).events;
    expect(parseAskEvent(raw!)).toEqual({ type: "token", text: "hello " });
  });

  it("parses a done event", () => {
    const [raw] = splitSSEBuffer(DONE_EVENT).events;
    expect(parseAskEvent(raw!)).toEqual({ type: "done", finish_reason: "stop" });
  });

  it("parses an error event", () => {
    const [raw] = splitSSEBuffer(ERROR_EVENT).events;
    expect(parseAskEvent(raw!)).toEqual({ type: "error", message: "boom" });
  });

  it("returns null for an unrecognized event name rather than throwing", () => {
    expect(parseAskEvent({ event: "ping", data: "" })).toBeNull();
  });

  it("returns null for malformed JSON rather than throwing", () => {
    expect(parseAskEvent({ event: "token", data: "{not json" })).toBeNull();
  });
});
