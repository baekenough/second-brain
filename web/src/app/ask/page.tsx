"use client";

import { useCallback, useEffect, useRef, useState, Suspense } from "react";
import { useRouter, useSearchParams, usePathname } from "next/navigation";
import { splitSSEBuffer, parseAskEvent } from "@/lib/sseEvents";
import {
  listConversations,
  getConversation,
  sendEvidenceFeedback,
  EvidenceFeedbackDisabledError,
} from "@/lib/api";
import type { AskSourceItem, AskConversationSummary, AskLayer, EvidenceVote } from "@/lib/types";
import { buildRankMap, nextVote, EVIDENCE_FEEDBACK_ENABLED } from "./evidenceFeedback";
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

// Render order for the evidence layers (spec §5.2, §7.2): the user's own
// notes first, then everything observed/collected, then LLM-derived
// inferences last (kept visually and positionally distinct).
const ASK_LAYER_ORDER: AskLayer[] = ["note", "observed", "insight"];

/** One thumbs button. Icon plus an explicit aria-label — the glyph alone does
 * not say which direction it is to a screen reader, and aria-pressed carries
 * the current vote so the state is not conveyed by colour only. */
function VoteButton({
  direction,
  active,
  onClick,
}: {
  direction: 1 | -1;
  active: boolean;
  onClick: () => void;
}) {
  const label = direction === 1 ? "이 근거가 도움이 됐어요" : "도움이 안 됐어요";
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      aria-label={label}
      title={label}
      className={`rounded p-1 transition-colors ${
        active ? "text-accent" : "text-foreground-subtle hover:text-foreground"
      }`}
    >
      <svg
        viewBox="0 0 20 20"
        fill="currentColor"
        aria-hidden="true"
        className={`h-3.5 w-3.5 ${direction === -1 ? "rotate-180" : ""}`}
      >
        <path d="M2.5 9.5A1.5 1.5 0 0 1 4 8h1.25v8H4a1.5 1.5 0 0 1-1.5-1.5v-5ZM6.75 8.2l3.1-4.65a1.2 1.2 0 0 1 2.2.9l-.65 2.8h3.55a1.6 1.6 0 0 1 1.56 1.95l-1.05 4.8A1.9 1.9 0 0 1 13.6 16H6.75V8.2Z" />
      </svg>
    </button>
  );
}

function SourceCard({
  source,
  rank,
  vote,
  failed,
  votingEnabled,
  onVote,
}: {
  source: AskSourceItem;
  /** 0-based position in the answer's evidence list, before layer grouping. */
  rank: number;
  vote: EvidenceVote | undefined;
  /** The last vote attempt for this card failed; shown as a quiet tint rather
   * than a toast, which would interrupt the conversation. */
  failed: boolean;
  votingEnabled: boolean;
  onVote: (documentId: string, layer: AskLayer, rank: number, clicked: 1 | -1) => void;
}) {
  const layer = getAskLayer(source.source_type);
  const isInsight = layer === "insight";
  return (
    <div
      className={`rounded-md border px-3 py-2 text-xs ${
        failed
          ? "border-danger/40 bg-surface-subtle"
          : isInsight
            ? "border-accent/30 bg-accent-subtle"
            : "border-border bg-surface-subtle"
      }`}
    >
      <div className="flex items-center gap-1.5">
        <span className={`font-medium ${isInsight ? "text-accent" : "text-foreground-subtle"}`}>
          {ASK_LAYER_LABELS[layer]}
        </span>
        <span className="line-clamp-1 text-foreground-muted">{source.title}</span>
        {votingEnabled && (
          <span className="ml-auto flex shrink-0 items-center gap-0.5">
            <VoteButton
              direction={1}
              active={vote === 1}
              onClick={() => onVote(source.id, layer, rank, 1)}
            />
            <VoteButton
              direction={-1}
              active={vote === -1}
              onClick={() => onVote(source.id, layer, rank, -1)}
            />
          </span>
        )}
      </div>
    </div>
  );
}

/** Collapsible group of source cards for a single evidence layer within one
 * answer turn. Collapsed by default (spec follow-up: evidence cards were
 * crowding the screen) — the caller owns the open/closed state so it can be
 * tracked per turn instead of globally. */
function SourceLayerGroup({
  layer,
  sources,
  open,
  onToggle,
  ranks,
  votes,
  failedDocIds,
  votingEnabled,
  onVote,
}: {
  layer: AskLayer;
  sources: AskSourceItem[];
  open: boolean;
  onToggle: () => void;
  /** document id → position in the unfiltered turn.sources array. */
  ranks: Record<string, number>;
  votes: Record<string, EvidenceVote>;
  failedDocIds: ReadonlySet<string>;
  votingEnabled: boolean;
  onVote: (documentId: string, layer: AskLayer, rank: number, clicked: 1 | -1) => void;
}) {
  return (
    <div className="rounded-md border border-border">
      <button
        type="button"
        onClick={onToggle}
        aria-expanded={open}
        className="flex w-full items-center justify-between gap-2 px-3 py-2 text-left text-xs font-medium text-foreground-muted transition-colors hover:text-foreground"
      >
        <span className="flex items-center gap-1.5">
          <span>{ASK_LAYER_LABELS[layer]}</span>
          <span className="rounded-full bg-surface-subtle px-1.5 py-0.5 text-[10px] text-foreground-subtle">
            {sources.length}
          </span>
        </span>
        <svg
          viewBox="0 0 20 20"
          fill="currentColor"
          aria-hidden="true"
          className={`h-3.5 w-3.5 shrink-0 text-foreground-subtle transition-transform duration-200 ${
            open ? "rotate-180" : ""
          }`}
        >
          <path
            fillRule="evenodd"
            d="M5.23 7.21a.75.75 0 0 1 1.06.02L10 11.168l3.71-3.938a.75.75 0 1 1 1.08 1.04l-4.25 4.5a.75.75 0 0 1-1.08 0l-4.25-4.5a.75.75 0 0 1 .02-1.06Z"
            clipRule="evenodd"
          />
        </svg>
      </button>
      {open && (
        <div className="flex flex-col gap-1.5 border-t border-border px-3 py-2">
          {sources.map((s) => (
            <SourceCard
              key={s.id}
              source={s}
              rank={ranks[s.id] ?? 0}
              vote={votes[s.id]}
              failed={failedDocIds.has(s.id)}
              votingEnabled={votingEnabled}
              onVote={onVote}
            />
          ))}
        </div>
      )}
    </div>
  );
}

function UserBubble({ question }: { question: string }) {
  return (
    <div className="flex justify-end">
      <div className="max-w-[85%] rounded-2xl rounded-br-sm bg-accent px-4 py-2.5 text-sm whitespace-pre-wrap text-white">
        {question}
      </div>
    </div>
  );
}

function AssistantBubble({
  turn,
  isLayerOpen,
  onToggleLayer,
  votes,
  failedDocIds,
  votingEnabled,
  onVote,
}: {
  turn: Turn;
  isLayerOpen: (layer: AskLayer) => boolean;
  onToggleLayer: (layer: AskLayer) => void;
  votes: Record<string, EvidenceVote>;
  failedDocIds: ReadonlySet<string>;
  votingEnabled: boolean;
  onVote: (documentId: string, layer: AskLayer, rank: number, clicked: 1 | -1) => void;
}) {
  const showTyping = turn.streaming && !turn.answer;

  // Ranks come from the unfiltered list and are computed BEFORE the layer
  // split: an index taken after grouping describes the group, not the position
  // the user actually saw.
  const ranks = buildRankMap(turn.sources);

  const sourcesByLayer = ASK_LAYER_ORDER.map((layer) => ({
    layer,
    sources: turn.sources.filter((s) => getAskLayer(s.source_type) === layer),
  })).filter((group) => group.sources.length > 0);

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
        {sourcesByLayer.length > 0 && (
          <div className="space-y-1.5">
            {sourcesByLayer.map(({ layer, sources }) => (
              <SourceLayerGroup
                key={layer}
                layer={layer}
                sources={sources}
                open={isLayerOpen(layer)}
                onToggle={() => onToggleLayer(layer)}
                ranks={ranks}
                votes={votes}
                failedDocIds={failedDocIds}
                votingEnabled={votingEnabled}
                onVote={onVote}
              />
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
      <Button
        type="button"
        variant="secondary"
        size="sm"
        onClick={onNew}
        className="justify-center"
      >
        + 새 대화
      </Button>
      <div className="flex-1 space-y-1 overflow-y-auto">
        {conversations.length === 0 && (
          <p className="px-2 py-4 text-center text-xs text-foreground-subtle">
            대화 기록이 없습니다
          </p>
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
  // Evidence-layer collapse state, keyed per turn so opening one turn's
  // "관찰 데이터" group never affects any other turn. Absence of a key means
  // collapsed — cards start hidden (spec follow-up: they crowded the screen).
  const [openLayers, setOpenLayers] = useState<Record<string, boolean>>({});

  const isLayerOpen = useCallback(
    (turnIndex: number, layer: AskLayer) => openLayers[`${turnIndex}:${layer}`] ?? false,
    [openLayers],
  );

  const toggleLayer = useCallback((turnIndex: number, layer: AskLayer) => {
    const key = `${turnIndex}:${layer}`;
    setOpenLayers((prev) => ({ ...prev, [key]: !prev[key] }));
  }, []);

  // Per-evidence thumbs, keyed `${turnIndex}:${documentId}`.
  //
  // Deliberate v1 limitation: existing votes are NOT read back from the
  // server, so reopening a conversation shows every card unvoted even where a
  // label was already recorded. The goal of this feature is to *collect*
  // labels, and the upsert is idempotent, so a re-vote costs nothing and adds
  // no rows. Restoring the display would need a read endpoint that returns
  // per-document votes for a conversation, which does not exist yet.
  const [votes, setVotes] = useState<Record<string, EvidenceVote>>({});
  // Cards whose last vote attempt failed. Rendered as a quiet border tint —
  // no toast, which would interrupt the conversation for a signal the user did
  // not ask for.
  const [failedVotes, setFailedVotes] = useState<Record<string, boolean>>({});
  // Flipped when the backend answers 404, i.e. FEEDBACK_EVIDENCE_ENABLED is
  // off while the frontend flag is on. The buttons then disappear for the rest
  // of the session instead of failing on every click.
  const [feedbackUnavailable, setFeedbackUnavailable] = useState(false);

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

  /** Records one thumbs judgement on one evidence card.
   *
   * Optimistic: the local value changes on click and is then OVERWRITTEN by
   * the server's answer, because the toggle rule lives in the upsert
   * (store.UpsertEvidence) and not here. On failure the previous value is
   * restored — replaying a failed call is safe, the upsert is idempotent per
   * (conversation, normalised query, document).
   *
   * The question comes from the turn, not from the input box: a card in an
   * older turn can be voted on at any time, and the label is only useful if it
   * is paired with the question that actually produced the card.
   *
   * This runs from an event handler, never from an effect (the repo's eslint
   * forbids setState inside effects). */
  async function handleVote(
    turn: Turn,
    documentId: string,
    layer: AskLayer,
    rank: number,
    clicked: 1 | -1,
  ) {
    // The backend requires a conversation id; before the first "conversation"
    // SSE event there is none, and the buttons are hidden in that state.
    if (!conversationId) return;

    const key = `${turn.turnIndex}:${documentId}`;
    const previous = votes[key];
    const optimistic = nextVote(previous, clicked);

    setVotes((prev) => ({ ...prev, [key]: optimistic }));
    setFailedVotes((prev) => (prev[key] ? { ...prev, [key]: false } : prev));

    try {
      const resolved = await sendEvidenceFeedback({
        conversation_id: conversationId,
        query: turn.question,
        document_id: documentId,
        thumbs: optimistic,
        rank,
        layer,
      });
      setVotes((prev) => ({ ...prev, [key]: resolved }));
    } catch (error) {
      // Roll back to the pre-click value.
      setVotes((prev) => {
        const next = { ...prev };
        if (previous === undefined) {
          delete next[key];
        } else {
          next[key] = previous;
        }
        return next;
      });
      if (error instanceof EvidenceFeedbackDisabledError) {
        // The server flag is off. Hide the buttons rather than letting every
        // click fail; this is a configuration state, not an error the user can
        // act on.
        setFeedbackUnavailable(true);
        return;
      }
      // No console.error: the error can carry the question or a document id.
      setFailedVotes((prev) => ({ ...prev, [key]: true }));
    }
  }

  function selectConversation(id: string) {
    if (streaming) abortControllerRef.current?.abort();
    setVotes({});
    setFailedVotes({});
    router.push(`${pathname}?c=${id}`, { scroll: false });
  }

  function startNewConversation() {
    if (streaming) abortControllerRef.current?.abort();
    setConversationId(null);
    setTurns([]);
    // Vote keys are turn-scoped, and turn indices restart in a new
    // conversation — keeping them would show another conversation's votes.
    setVotes({});
    setFailedVotes({});
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

  // Three gates, all of which must hold: the build-time frontend flag (default
  // off), no observed 404 from the server flag, and a conversation id to
  // attach the label to.
  const votingEnabled =
    EVIDENCE_FEEDBACK_ENABLED && !feedbackUnavailable && conversationId !== null;

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
            <p className="py-12 text-center text-sm text-foreground-subtle">대화를 불러오는 중…</p>
          )}
          {isEmpty && (
            <p className="py-12 text-center text-sm text-foreground-subtle">
              내 데이터에 질문해보세요
            </p>
          )}
          {turns.map((turn) => {
            // Vote state is stored flat and keyed `${turnIndex}:${documentId}`;
            // narrow it to this turn so the cards only need document ids.
            const prefix = `${turn.turnIndex}:`;
            const turnVotes: Record<string, EvidenceVote> = {};
            for (const [key, value] of Object.entries(votes)) {
              if (key.startsWith(prefix)) turnVotes[key.slice(prefix.length)] = value;
            }
            const turnFailed = new Set(
              Object.keys(failedVotes)
                .filter((key) => failedVotes[key] && key.startsWith(prefix))
                .map((key) => key.slice(prefix.length)),
            );
            return (
              <div key={turn.turnIndex} className="space-y-3">
                <UserBubble question={turn.question} />
                <AssistantBubble
                  turn={turn}
                  isLayerOpen={(layer) => isLayerOpen(turn.turnIndex, layer)}
                  onToggleLayer={(layer) => toggleLayer(turn.turnIndex, layer)}
                  votes={turnVotes}
                  failedDocIds={turnFailed}
                  votingEnabled={votingEnabled}
                  onVote={(documentId, layer, rank, clicked) =>
                    void handleVote(turn, documentId, layer, rank, clicked)
                  }
                />
              </div>
            );
          })}
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
