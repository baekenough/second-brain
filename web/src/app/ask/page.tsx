"use client";

import { useCallback, useEffect, useRef, useState, Suspense } from "react";
import { useRouter, useSearchParams, usePathname } from "next/navigation";
import { splitSSEBuffer, parseAskEvent } from "@/lib/sseEvents";
import { listConversations, getConversation } from "@/lib/api";
import type { AskSourceItem, AskConversationSummary } from "@/lib/types";
import { getAskLayer, ASK_LAYER_LABELS } from "@/lib/constants";
import { Button } from "@/components/ui";
import { MarkdownContent } from "@/app/documents/[id]/MarkdownContent";

type FinishReason = "stop" | "error" | "no_evidence";

/** One rendered exchange: the user's question + the assistant's (possibly
 * still-streaming) answer. Mirrors one row of ask_sessions (internal/store/
 * ask_sessions.go's AskSession), plus local-only streaming/error state. */
interface Turn {
  turnIndex: number;
  question: string;
  answer: string;
  sources: AskSourceItem[];
  finishReason: FinishReason | null;
  streaming: boolean;
  error: string | null;
}

// How often accumulated "token" events are flushed into React state while a
// turn is streaming. Re-parsing the full markdown answer on every single
// token (which can arrive many times per second) would make the page
// stutter; batching flushes to roughly this interval keeps the UI smooth
// without a noticeably laggy typing effect.
const STREAM_FLUSH_INTERVAL_MS = 80;

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

function UserBubble({ question }: { question: string }) {
  return (
    <div className="flex justify-end">
      <div className="max-w-[85%] rounded-2xl rounded-br-sm bg-accent px-4 py-2.5 text-sm text-white whitespace-pre-wrap">
        {question}
      </div>
    </div>
  );
}

function AssistantBubble({ turn }: { turn: Turn }) {
  const showTyping = turn.streaming && !turn.answer;
  return (
    <div className="flex justify-start">
      <div className="max-w-[90%] space-y-2">
        <div className="rounded-2xl rounded-bl-sm border border-border bg-surface px-4 py-2.5">
          {turn.answer && <MarkdownContent source={turn.answer} />}
          {showTyping && (
            <div className="flex items-center gap-1 py-1" aria-label="답변 생성 중">
              <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-foreground-subtle [animation-delay:-0.3s]" />
              <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-foreground-subtle [animation-delay:-0.15s]" />
              <span className="h-1.5 w-1.5 animate-bounce rounded-full bg-foreground-subtle" />
            </div>
          )}
          {turn.finishReason === "no_evidence" && (
            <p className="text-sm text-foreground-subtle">관련된 근거를 찾지 못했습니다.</p>
          )}
          {turn.error && <p className="text-sm text-danger">{turn.error}</p>}
        </div>
        {turn.sources.length > 0 && (
          <div className="flex flex-wrap gap-1.5">
            {turn.sources.map((s) => (
              <SourceCard key={s.id} source={s} />
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

function ConversationSidebar({
  conversations,
  activeId,
  onSelect,
  onNew,
}: {
  conversations: AskConversationSummary[];
  activeId: string | null;
  onSelect: (id: string) => void;
  onNew: () => void;
}) {
  return (
    <aside className="hidden w-64 shrink-0 flex-col gap-2 border-r border-border pr-3 lg:flex">
      <Button type="button" variant="secondary" size="sm" onClick={onNew} className="justify-center">
        + 새 대화
      </Button>
      <div className="flex-1 space-y-1 overflow-y-auto">
        {conversations.length === 0 && (
          <p className="px-2 py-4 text-center text-xs text-foreground-subtle">대화 기록이 없습니다</p>
        )}
        {conversations.map((c) => (
          <button
            key={c.conversation_id}
            type="button"
            onClick={() => onSelect(c.conversation_id)}
            className={`block w-full truncate rounded-md px-2 py-2 text-left text-xs transition-colors ${
              c.conversation_id === activeId
                ? "bg-accent-subtle text-accent"
                : "text-foreground-muted hover:bg-surface-subtle hover:text-foreground"
            }`}
          >
            {c.question || "(제목 없음)"}
          </button>
        ))}
      </div>
    </aside>
  );
}

function AskPageInner() {
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const conversationParam = searchParams.get("c");

  const [question, setQuestion] = useState("");
  const [turns, setTurns] = useState<Turn[]>([]);
  const [conversationId, setConversationId] = useState<string | null>(null);
  const [conversations, setConversations] = useState<AskConversationSummary[]>([]);
  const [streaming, setStreaming] = useState(false);
  const [restoring, setRestoring] = useState(false);

  const bufferRef = useRef("");
  const pendingTextRef = useRef("");
  const flushTimerRef = useRef<ReturnType<typeof setInterval> | null>(null);
  // Tracks the in-flight request so a new question (or unmount) can cancel
  // whatever stream is currently being read — without this, asking a
  // second question while the first is still streaming would leave both
  // readers appending tokens into the same turn state concurrently.
  const abortControllerRef = useRef<AbortController | null>(null);
  const scrollEndRef = useRef<HTMLDivElement | null>(null);

  const loadConversations = useCallback(() => {
    listConversations(20)
      .then(setConversations)
      .catch(() => setConversations([]));
  }, []);

  useEffect(() => {
    loadConversations();
  }, [loadConversations]);

  // Restore a conversation from the URL (?c=<uuid>) on load or when the
  // param changes externally (sidebar click updates it too, but that path
  // sets local state directly — see selectConversation — so this effect
  // mainly covers page load / back-forward navigation / a shared link).
  useEffect(() => {
    if (!conversationParam) {
      setTurns([]);
      setConversationId(null);
      return;
    }
    if (conversationParam === conversationId) return;

    let cancelled = false;
    setRestoring(true);
    getConversation(conversationParam)
      .then((rows) => {
        if (cancelled) return;
        setConversationId(conversationParam);
        setTurns(
          rows.map((t) => ({
            turnIndex: t.turn_index,
            question: t.question,
            answer: t.answer,
            sources: t.sources ?? [],
            finishReason: t.finish_reason,
            streaming: false,
            error: null,
          })),
        );
      })
      .catch(() => {
        if (cancelled) return;
        // Conversation no longer exists / failed to load — fall back to a
        // fresh conversation rather than leaving the screen stuck.
        setConversationId(null);
        setTurns([]);
      })
      .finally(() => {
        if (!cancelled) setRestoring(false);
      });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [conversationParam]);

  useEffect(() => {
    scrollEndRef.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [turns]);

  useEffect(() => {
    return () => {
      abortControllerRef.current?.abort();
      if (flushTimerRef.current) clearInterval(flushTimerRef.current);
    };
  }, []);

  function updateLastTurn(updater: (turn: Turn) => Turn) {
    setTurns((prev) => {
      if (prev.length === 0) return prev;
      const next = [...prev];
      const last = next[next.length - 1];
      if (!last) return prev;
      next[next.length - 1] = updater(last);
      return next;
    });
  }

  function startFlushTimer() {
    if (flushTimerRef.current) return;
    flushTimerRef.current = setInterval(() => {
      if (!pendingTextRef.current) return;
      const chunk = pendingTextRef.current;
      pendingTextRef.current = "";
      updateLastTurn((t) => ({ ...t, answer: t.answer + chunk }));
    }, STREAM_FLUSH_INTERVAL_MS);
  }

  function stopFlushTimer() {
    if (flushTimerRef.current) {
      clearInterval(flushTimerRef.current);
      flushTimerRef.current = null;
    }
    // Flush whatever is left so the final characters are not dropped.
    if (pendingTextRef.current) {
      const chunk = pendingTextRef.current;
      pendingTextRef.current = "";
      updateLastTurn((t) => ({ ...t, answer: t.answer + chunk }));
    }
  }

  function selectConversation(id: string) {
    if (streaming) abortControllerRef.current?.abort();
    router.push(`${pathname}?c=${id}`, { scroll: false });
  }

  function startNewConversation() {
    if (streaming) abortControllerRef.current?.abort();
    setConversationId(null);
    setTurns([]);
    router.push(pathname, { scroll: false });
  }

  async function handleAsk() {
    const trimmed = question.trim();
    if (!trimmed || streaming) return;

    // Cancel any previous in-flight stream before starting a new one.
    abortControllerRef.current?.abort();
    const controller = new AbortController();
    abortControllerRef.current = controller;

    setQuestion("");
    setStreaming(true);
    bufferRef.current = "";
    pendingTextRef.current = "";
    startFlushTimer();

    setTurns((prev) => [
      ...prev,
      {
        turnIndex: prev.length,
        question: trimmed,
        answer: "",
        sources: [],
        finishReason: null,
        streaming: true,
        error: null,
      },
    ]);

    try {
      const res = await fetch("/api/ask", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          question: trimmed,
          ...(conversationId && { conversation_id: conversationId }),
        }),
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
          if (evt.type === "conversation") {
            setConversationId((prev) => {
              if (prev === evt.conversation_id) return prev;
              // First turn of a brand-new conversation: reflect its id in
              // the URL so a refresh restores it (spec requirement).
              router.replace(`${pathname}?c=${evt.conversation_id}`, { scroll: false });
              return evt.conversation_id;
            });
          } else if (evt.type === "sources") {
            updateLastTurn((t) => ({ ...t, sources: evt.sources }));
          } else if (evt.type === "token") {
            pendingTextRef.current += evt.text;
          } else if (evt.type === "error") {
            updateLastTurn((t) => ({ ...t, error: evt.message }));
          } else if (evt.type === "done") {
            updateLastTurn((t) => ({ ...t, finishReason: evt.finish_reason }));
          }
        }
      }
    } catch (e: unknown) {
      // A deliberate abort (new question / navigating away) is not a user
      // facing error — surfacing it would flash a confusing message every
      // time the user asks a follow-up before the previous answer finishes.
      if (e instanceof DOMException && e.name === "AbortError") return;
      updateLastTurn((t) => ({
        ...t,
        error: e instanceof Error ? e.message : "답변 생성 중 오류가 발생했습니다.",
      }));
    } finally {
      stopFlushTimer();
      if (abortControllerRef.current === controller) {
        setStreaming(false);
        updateLastTurn((t) => ({ ...t, streaming: false }));
        abortControllerRef.current = null;
        loadConversations();
      }
    }
  }

  const isEmpty = turns.length === 0 && !restoring;

  return (
    <div className="flex min-h-[calc(100vh-8rem)] gap-4">
      <ConversationSidebar
        conversations={conversations}
        activeId={conversationId}
        onSelect={selectConversation}
        onNew={startNewConversation}
      />

      <div className="flex min-w-0 flex-1 flex-col">
        {/* Conversation */}
        <div className="flex-1 space-y-4 overflow-y-auto pb-24 sm:pb-4">
          {restoring && (
            <p className="py-12 text-center text-sm text-foreground-subtle">
              대화를 불러오는 중…
            </p>
          )}
          {isEmpty && (
            <p className="py-12 text-center text-sm text-foreground-subtle">
              내 데이터에 질문해보세요
            </p>
          )}
          {turns.map((turn) => (
            <div key={turn.turnIndex} className="space-y-3">
              <UserBubble question={turn.question} />
              <AssistantBubble turn={turn} />
            </div>
          ))}
          <div ref={scrollEndRef} />
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
              className="min-h-[44px] flex-1 rounded-lg border border-border bg-surface px-3 py-2.5 text-sm text-foreground placeholder:text-foreground-subtle focus:ring-2 focus:ring-accent/40 focus:outline-none disabled:opacity-50"
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
    </div>
  );
}

export default function AskPage() {
  return (
    <Suspense>
      <AskPageInner />
    </Suspense>
  );
}
