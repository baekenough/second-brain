"use client";

import { useEffect, useRef, useState } from "react";
import { splitSSEBuffer, parseAskEvent } from "@/lib/sseEvents";
import type { AskSourceItem } from "@/lib/types";
import { getAskLayer, ASK_LAYER_LABELS } from "@/lib/constants";
import { Button } from "@/components/ui";

function SourceCard({ source }: { source: AskSourceItem }) {
  const layer = getAskLayer(source.source_type);
  const isInsight = layer === "insight";
  return (
    <div
      className={`rounded-md border px-3 py-2 text-xs ${
        isInsight ? "border-accent/30 bg-accent-subtle" : "border-border bg-surface-subtle"
      }`}
    >
      <div className="flex items-center gap-1.5">
        <span className={`font-medium ${isInsight ? "text-accent" : "text-foreground-subtle"}`}>
          {ASK_LAYER_LABELS[layer]}
        </span>
        <span className="line-clamp-1 text-foreground-muted">{source.title}</span>
      </div>
    </div>
  );
}

export default function AskPage() {
  const [question, setQuestion] = useState("");
  const [answer, setAnswer] = useState("");
  const [sources, setSources] = useState<AskSourceItem[]>([]);
  const [streaming, setStreaming] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const bufferRef = useRef("");
  // Tracks the in-flight request so a new question (or unmount) can cancel
  // whatever stream is currently being read — without this, asking a
  // second question while the first is still streaming would leave both
  // readers appending tokens into the same answer state concurrently.
  const abortControllerRef = useRef<AbortController | null>(null);

  useEffect(() => {
    return () => {
      abortControllerRef.current?.abort();
    };
  }, []);

  async function handleAsk() {
    const trimmed = question.trim();
    if (!trimmed || streaming) return;

    // Cancel any previous in-flight stream before starting a new one.
    abortControllerRef.current?.abort();
    const controller = new AbortController();
    abortControllerRef.current = controller;

    setAnswer("");
    setSources([]);
    setError(null);
    setStreaming(true);
    bufferRef.current = "";

    try {
      const res = await fetch("/api/ask", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ question: trimmed }),
        signal: controller.signal,
      });

      if (!res.ok || !res.body) {
        const body = (await res.json().catch(() => ({}))) as { error?: string };
        throw new Error(body.error ?? `요청이 실패했습니다 (${res.status})`);
      }

      const reader = res.body.getReader();
      const decoder = new TextDecoder();

      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;

        bufferRef.current += decoder.decode(value, { stream: true });
        const { events, remainder } = splitSSEBuffer(bufferRef.current);
        bufferRef.current = remainder;

        for (const raw of events) {
          const evt = parseAskEvent(raw);
          if (!evt) continue;
          if (evt.type === "sources") setSources(evt.sources);
          else if (evt.type === "token") setAnswer((prev) => prev + evt.text);
          else if (evt.type === "error") setError(evt.message);
          else if (evt.type === "done" && evt.finish_reason === "no_evidence") {
            setError((prev) => prev ?? "관련된 근거를 찾지 못했습니다.");
          }
        }
      }
    } catch (e: unknown) {
      // A deliberate abort (new question / navigating away) is not a user
      // facing error — surfacing it would flash a confusing message every
      // time the user asks a follow-up before the previous answer finishes.
      if (e instanceof DOMException && e.name === "AbortError") return;
      setError(e instanceof Error ? e.message : "답변 생성 중 오류가 발생했습니다.");
    } finally {
      if (abortControllerRef.current === controller) {
        setStreaming(false);
        abortControllerRef.current = null;
      }
    }
  }

  return (
    <div className="flex min-h-[calc(100vh-8rem)] flex-col">
      {/* Answer + sources */}
      <div className="flex-1 space-y-4 overflow-y-auto pb-24 sm:pb-4">
        {sources.length > 0 && (
          <div className="flex flex-wrap gap-1.5">
            {sources.map((s) => (
              <SourceCard key={s.id} source={s} />
            ))}
          </div>
        )}
        {answer && (
          <p className="text-sm leading-relaxed whitespace-pre-wrap text-foreground">{answer}</p>
        )}
        {error && <p className="text-sm text-danger">{error}</p>}
        {!answer && !error && !streaming && (
          <p className="py-12 text-center text-sm text-foreground-subtle">
            내 데이터에 질문해보세요
          </p>
        )}
      </div>

      {/* Input — mobile: fixed to the viewport bottom (spec §7.2's required
          deviation). This app has a real prior incident with an element
          fixed to the bottom edge overlapping content underneath it (a
          save button hidden behind the tab bar on a past mobile release) —
          the pb-24 on the scroll container above exists specifically to
          reserve room for this bar so the same class of bug does not repeat
          here. Desktop: inline, no fixed positioning needed. */}
      <div className="fixed inset-x-0 bottom-0 border-t border-border bg-background px-4 py-3 sm:static sm:border-0 sm:bg-transparent sm:px-0 sm:py-0">
        <div className="mx-auto flex max-w-4xl gap-2">
          <input
            type="text"
            value={question}
            onChange={(e) => setQuestion(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                void handleAsk();
              }
            }}
            placeholder="질문을 입력하세요…"
            disabled={streaming}
            className="flex-1 rounded-lg border border-border bg-surface px-3 py-2.5 text-sm text-foreground placeholder:text-foreground-subtle focus:ring-2 focus:ring-accent/40 focus:outline-none disabled:opacity-50"
          />
          <Button
            type="button"
            variant="primary"
            onClick={() => void handleAsk()}
            loading={streaming}
            disabled={streaming || !question.trim()}
          >
            질문
          </Button>
        </div>
      </div>
    </div>
  );
}
