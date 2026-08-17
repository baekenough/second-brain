import type { AskStreamEvent, AskSourceItem } from "./types";

export interface RawSSEEvent {
  event: string;
  data: string;
}

/**
 * Splits accumulated SSE text on the "\n\n" event delimiter (spec §5.1's
 * wire format). fetch's ReadableStream delivers arbitrary byte boundaries
 * that do not line up with SSE event boundaries — callers must keep
 * `remainder` and prepend it to the next chunk before calling this again.
 */
export function splitSSEBuffer(buffer: string): { events: RawSSEEvent[]; remainder: string } {
  const parts = buffer.split("\n\n");
  const remainder = parts.pop() ?? "";
  const events: RawSSEEvent[] = [];

  for (const part of parts) {
    if (!part.trim()) continue;
    let event = "message";
    const dataLines: string[] = [];
    for (const line of part.split("\n")) {
      if (line.startsWith("event:")) {
        event = line.slice("event:".length).trim();
      } else if (line.startsWith("data:")) {
        dataLines.push(line.slice("data:".length).trim());
      }
    }
    events.push({ event, data: dataLines.join("\n") });
  }

  return { events, remainder };
}

/**
 * Maps a raw SSE event into a typed AskStreamEvent per the spec §5.1
 * contract. Returns null (never throws) for unrecognized event names or
 * malformed JSON payloads — a single bad event must not crash the whole
 * stream reader loop (Task 9).
 */
export function parseAskEvent(raw: RawSSEEvent): AskStreamEvent | null {
  try {
    switch (raw.event) {
      case "conversation": {
        const payload = JSON.parse(raw.data) as { conversation_id: string; turn_index: number };
        return {
          type: "conversation",
          conversation_id: payload.conversation_id,
          turn_index: payload.turn_index,
        };
      }
      case "sources": {
        const payload = JSON.parse(raw.data) as { sources: AskSourceItem[] };
        return { type: "sources", sources: payload.sources ?? [] };
      }
      case "token": {
        const payload = JSON.parse(raw.data) as { text: string };
        return { type: "token", text: payload.text ?? "" };
      }
      case "done": {
        const payload = JSON.parse(raw.data) as {
          finish_reason: "stop" | "error" | "no_evidence";
        };
        return { type: "done", finish_reason: payload.finish_reason };
      }
      case "error": {
        const payload = JSON.parse(raw.data) as { message: string };
        return { type: "error", message: payload.message ?? "unknown error" };
      }
      default:
        return null;
    }
  } catch {
    return null;
  }
}
