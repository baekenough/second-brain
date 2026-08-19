package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// --- (a) first question skips rewrite ---

// TestAskHandler_FirstTurn_SkipsRewrite covers requirement (a): a brand-new
// conversation (no conversation_id, empty history) must not invoke the
// rewrite LLM call at all — only the streaming synthesis call.
func TestAskHandler_FirstTurn_SkipsRewrite(t *testing.T) {
	t.Parallel()
	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문서")}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"답"}}
	sessions := newFakeAskSessionStore()
	srv := newAskTestServerWithSessions(searcher, &fakeIntentClassifier{}, fakeLLM, sessions)

	rr := doAskRequest(t, srv, nil, map[string]any{"question": "첫 질문입니다"}, "Bearer test-key")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if fakeLLM.completeCalls != 0 {
		t.Errorf("CompleteWithMessages (rewrite) called %d times on the first turn, want 0", fakeLLM.completeCalls)
	}
	if len(searcher.gotQueries) == 0 || searcher.gotQueries[0] != "첫 질문입니다" {
		t.Errorf("search query = %v, want the raw question unchanged (no history to rewrite from)", searcher.gotQueries)
	}
}

// --- (b) follow-up searches the rewritten query ---

// TestAskHandler_FollowUp_SearchesRewrittenQuery covers requirement (b): a
// second turn in an existing conversation must classify/search using the
// LLM's rewritten standalone question, not the user's raw follow-up
// wording — and the rewrite call itself must be given the prior turn's
// question/answer text.
func TestAskHandler_FollowUp_SearchesRewrittenQuery(t *testing.T) {
	t.Parallel()
	conversationID := uuid.New()
	sessions := newFakeAskSessionStore()
	sessions.seed(conversationID, store.AskSession{
		ID: uuid.New(), ConversationID: conversationID, TurnIndex: 0,
		Question: "second-brain 프로젝트가 뭐야", Answer: "개인 지식 관리 백엔드 서비스입니다",
		FinishReason: "stop",
	})

	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문서")}}
	fakeLLM := &fakeAskLLM{
		enabled:      true,
		chunks:       []string{"답"},
		completeResp: "second-brain 프로젝트의 아키텍처는 어떻게 되나요",
	}
	srv := newAskTestServerWithSessions(searcher, &fakeIntentClassifier{}, fakeLLM, sessions)

	rr := doAskRequest(t, srv, nil, map[string]any{
		"question":        "그거 더 자세히 알려줘",
		"conversation_id": conversationID.String(),
	}, "Bearer test-key")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if fakeLLM.completeCalls != 1 {
		t.Fatalf("CompleteWithMessages (rewrite) called %d times, want exactly 1", fakeLLM.completeCalls)
	}
	if len(searcher.gotQueries) == 0 {
		t.Fatal("searcher was never called")
	}
	for _, q := range searcher.gotQueries {
		if q != fakeLLM.completeResp {
			t.Errorf("search query = %q, want the rewritten question %q (not the raw follow-up)", q, fakeLLM.completeResp)
		}
	}

	rewritePrompt := fakeLLM.completeMessagesLog[0]
	if len(rewritePrompt) == 0 {
		t.Fatal("rewrite call received no messages")
	}
	rewriteContent := rewritePrompt[0].Content
	if !strings.Contains(rewriteContent, "second-brain 프로젝트가 뭐야") || !strings.Contains(rewriteContent, "개인 지식 관리 백엔드 서비스입니다") {
		t.Errorf("rewrite prompt = %q, want it to include the previous turn's question and answer", rewriteContent)
	}
	if !strings.Contains(rewriteContent, "그거 더 자세히 알려줘") {
		t.Errorf("rewrite prompt = %q, want it to include the current raw question", rewriteContent)
	}
}

// TestAskHandler_FollowUp_SynthesisSeesOriginalQuestion asserts Stage 3
// still shows the model the user's ORIGINAL wording as the current turn,
// not the rewritten standalone question — only search/classification use
// the rewrite (spec: "종합 단계에는 사용자의 원래 질문을 보여주세요").
func TestAskHandler_FollowUp_SynthesisSeesOriginalQuestion(t *testing.T) {
	t.Parallel()
	conversationID := uuid.New()
	sessions := newFakeAskSessionStore()
	sessions.seed(conversationID, store.AskSession{
		ID: uuid.New(), ConversationID: conversationID, TurnIndex: 0,
		Question: "이전 질문", Answer: "이전 답변", FinishReason: "stop",
	})

	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문서")}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"답"}, completeResp: "재작성된 질문"}
	srv := newAskTestServerWithSessions(searcher, &fakeIntentClassifier{}, fakeLLM, sessions)

	rr := doAskRequest(t, srv, nil, map[string]any{
		"question":        "원래 질문 그대로",
		"conversation_id": conversationID.String(),
	}, "Bearer test-key")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	// fakeLLM.gotMessages is overwritten last by the streaming synthesis
	// call (rewrite uses CompleteWithMessages, synthesis uses
	// StreamWithMessages), so at this point it reflects Stage 3 only.
	lastMsg := fakeLLM.gotMessages[len(fakeLLM.gotMessages)-1]
	if lastMsg.Content != "원래 질문 그대로" {
		t.Errorf("synthesis final turn = %q, want the user's original wording %q (not the rewritten %q)",
			lastMsg.Content, "원래 질문 그대로", fakeLLM.completeResp)
	}
}

// --- (c) synthesis prompt excludes prior source-document bodies ---

// TestAskHandler_SynthesisPrompt_ExcludesPriorSourceDocumentBodies covers
// requirement (c): the previous turn's Sources (evidence documents) must
// never be replayed into the Stage 3 prompt — only its question/answer
// TEXT. A distinctive marker is planted only in the previous turn's stored
// Sources (never in its Question/Answer), so its presence in the prompt
// sent to the LLM would prove a leak.
func TestAskHandler_SynthesisPrompt_ExcludesPriorSourceDocumentBodies(t *testing.T) {
	t.Parallel()
	const leakMarker = "이전턴근거문서제목마커12345"

	conversationID := uuid.New()
	sessions := newFakeAskSessionStore()
	sessions.seed(conversationID, store.AskSession{
		ID: uuid.New(), ConversationID: conversationID, TurnIndex: 0,
		Question: "이전 질문 텍스트", Answer: "이전 답변 텍스트", FinishReason: "stop",
		Sources: []store.AskSource{{ID: uuid.New().String(), Title: leakMarker, SourceType: "sms", Score: 0.9}},
	})

	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "새 문서")}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"답"}, completeResp: "재작성됨"}
	srv := newAskTestServerWithSessions(searcher, &fakeIntentClassifier{}, fakeLLM, sessions)

	rr := doAskRequest(t, srv, nil, map[string]any{
		"question":        "후속 질문",
		"conversation_id": conversationID.String(),
	}, "Bearer test-key")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	var allContent strings.Builder
	for _, m := range fakeLLM.gotMessages {
		allContent.WriteString(m.Content)
		allContent.WriteString("\n")
	}
	if strings.Contains(allContent.String(), leakMarker) {
		t.Errorf("synthesis prompt leaked a prior turn's source-document title: %q\nfull prompt messages: %#v", leakMarker, fakeLLM.gotMessages)
	}
	if !strings.Contains(allContent.String(), "이전 질문 텍스트") || !strings.Contains(allContent.String(), "이전 답변 텍스트") {
		t.Errorf("synthesis prompt is missing the previous turn's question/answer text entirely: %#v", fakeLLM.gotMessages)
	}
}

// --- (d) save failure must not break the response ---

// TestAskHandler_SaveFailure_DoesNotBreakResponse covers requirement (d):
// an AskSessionStore.Insert failure must be logged and swallowed, never
// surfaced to the client — the SSE event sequence must be identical to the
// happy path.
func TestAskHandler_SaveFailure_DoesNotBreakResponse(t *testing.T) {
	t.Parallel()
	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문서")}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"안녕", "하세요"}}
	sessions := newFakeAskSessionStore()
	sessions.insertErr = errors.New("db: connection reset")
	srv := newAskTestServerWithSessions(searcher, &fakeIntentClassifier{}, fakeLLM, sessions)

	rr := doAskRequest(t, srv, nil, map[string]any{"question": "질문"}, "Bearer test-key")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	frames := parseSSEFrames(t, rr.Body.String())
	got := frameEvents(frames)
	want := []string{"conversation", "sources", "token", "token", "done"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v (a store failure must not change the SSE sequence)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("events[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	var doneP askDonePayload
	if err := json.Unmarshal([]byte(frames[len(frames)-1].data), &doneP); err != nil {
		t.Fatalf("unmarshal done payload: %v", err)
	}
	if doneP.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want %q despite the store failure", doneP.FinishReason, "stop")
	}
	if len(sessions.insertCalls) != 1 {
		t.Errorf("Insert was attempted %d times, want exactly 1 (it should still be attempted, just tolerated on failure)", len(sessions.insertCalls))
	}
}

// --- (e) "conversation" event precedes "sources" ---

// TestAskHandler_ConversationEventPrecedesSources covers requirement (e)
// end-to-end (TestAskHandler_SSEFieldNames also checks this incidentally;
// this test is the dedicated, explicitly-named assertion for it) and
// additionally asserts the payload's conversation_id/turn_index values for
// a brand-new conversation.
func TestAskHandler_ConversationEventPrecedesSources(t *testing.T) {
	t.Parallel()
	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문서")}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"답"}}
	sessions := newFakeAskSessionStore()
	srv := newAskTestServerWithSessions(searcher, &fakeIntentClassifier{}, fakeLLM, sessions)

	rr := doAskRequest(t, srv, nil, map[string]any{"question": "질문"}, "Bearer test-key")

	frames := parseSSEFrames(t, rr.Body.String())
	if len(frames) < 2 {
		t.Fatalf("too few frames: %v", frameEvents(frames))
	}
	if frames[0].event != "conversation" {
		t.Fatalf("frames[0].event = %q, want %q", frames[0].event, "conversation")
	}
	if frames[1].event != "sources" {
		t.Fatalf("frames[1].event = %q, want %q", frames[1].event, "sources")
	}

	var payload askConversationPayload
	if err := json.Unmarshal([]byte(frames[0].data), &payload); err != nil {
		t.Fatalf("unmarshal conversation payload: %v", err)
	}
	if _, err := uuid.Parse(payload.ConversationID); err != nil {
		t.Errorf("conversation_id = %q, not a valid uuid: %v", payload.ConversationID, err)
	}
	if payload.TurnIndex != 0 {
		t.Errorf("turn_index = %d, want 0 for a brand-new conversation", payload.TurnIndex)
	}
}

// --- conversation_id round-trip across two real requests ---

// TestAskHandler_ConversationRoundTrip_TurnIndexIncrements drives two
// sequential requests through the same in-memory session store: the first
// creates a conversation, the second supplies its conversation_id back and
// must be assigned turn_index 1 (not another turn_index 0).
func TestAskHandler_ConversationRoundTrip_TurnIndexIncrements(t *testing.T) {
	t.Parallel()
	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문서")}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"답1"}}
	sessions := newFakeAskSessionStore()
	srv := newAskTestServerWithSessions(searcher, &fakeIntentClassifier{}, fakeLLM, sessions)

	rr1 := doAskRequest(t, srv, nil, map[string]any{"question": "첫 질문"}, "Bearer test-key")
	frames1 := parseSSEFrames(t, rr1.Body.String())
	var conv1 askConversationPayload
	if err := json.Unmarshal([]byte(frames1[0].data), &conv1); err != nil {
		t.Fatalf("unmarshal first conversation payload: %v", err)
	}
	if conv1.TurnIndex != 0 {
		t.Fatalf("first turn_index = %d, want 0", conv1.TurnIndex)
	}

	fakeLLM.chunks = []string{"답2"}
	rr2 := doAskRequest(t, srv, nil, map[string]any{
		"question":        "두번째 질문",
		"conversation_id": conv1.ConversationID,
	}, "Bearer test-key")
	frames2 := parseSSEFrames(t, rr2.Body.String())
	var conv2 askConversationPayload
	if err := json.Unmarshal([]byte(frames2[0].data), &conv2); err != nil {
		t.Fatalf("unmarshal second conversation payload: %v", err)
	}
	if conv2.ConversationID != conv1.ConversationID {
		t.Errorf("second conversation_id = %q, want it to match the first %q", conv2.ConversationID, conv1.ConversationID)
	}
	if conv2.TurnIndex != 1 {
		t.Errorf("second turn_index = %d, want 1", conv2.TurnIndex)
	}
}

// TestAskHandler_UnparsableConversationID_StartsFreshConversation asserts
// an unparsable conversation_id degrades to "start a new conversation"
// rather than a 400 — a stale/mistyped client-supplied id must not block
// the user from asking a question.
func TestAskHandler_UnparsableConversationID_StartsFreshConversation(t *testing.T) {
	t.Parallel()
	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문서")}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"답"}}
	sessions := newFakeAskSessionStore()
	srv := newAskTestServerWithSessions(searcher, &fakeIntentClassifier{}, fakeLLM, sessions)

	rr := doAskRequest(t, srv, nil, map[string]any{
		"question":        "질문",
		"conversation_id": "not-a-uuid",
	}, "Bearer test-key")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	frames := parseSSEFrames(t, rr.Body.String())
	var payload askConversationPayload
	if err := json.Unmarshal([]byte(frames[0].data), &payload); err != nil {
		t.Fatalf("unmarshal conversation payload: %v", err)
	}
	if payload.ConversationID == "not-a-uuid" {
		t.Errorf("conversation_id echoed back the unparsable client value instead of minting a new one")
	}
	if _, err := uuid.Parse(payload.ConversationID); err != nil {
		t.Errorf("conversation_id = %q, not a valid uuid: %v", payload.ConversationID, err)
	}
	if payload.TurnIndex != 0 {
		t.Errorf("turn_index = %d, want 0", payload.TurnIndex)
	}
}

// --- no persistence wired: single-turn behaviour preserved ---

// TestAskHandler_NoSessionsWired_StillAnswersSingleTurn asserts a Server
// that never calls WithAskSessions still fully answers /ask (the
// "conversation" event is still emitted with a fresh id every time,
// turn_index is always 0, and no rewrite/save ever happens since there is
// no history to read).
func TestAskHandler_NoSessionsWired_StillAnswersSingleTurn(t *testing.T) {
	t.Parallel()
	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문서")}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"답"}}
	srv := newAskTestServer(searcher, &fakeIntentClassifier{}, fakeLLM) // no WithAskSessions

	rr := doAskRequest(t, srv, nil, map[string]any{
		"question":        "질문",
		"conversation_id": uuid.New().String(),
	}, "Bearer test-key")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	frames := parseSSEFrames(t, rr.Body.String())
	var payload askConversationPayload
	if err := json.Unmarshal([]byte(frames[0].data), &payload); err != nil {
		t.Fatalf("unmarshal conversation payload: %v", err)
	}
	if payload.TurnIndex != 0 {
		t.Errorf("turn_index = %d, want 0 (no persistence wired, so history can never be read back)", payload.TurnIndex)
	}
	if fakeLLM.completeCalls != 0 {
		t.Errorf("rewrite (CompleteWithMessages) called %d times with no persistence wired, want 0", fakeLLM.completeCalls)
	}
}

// --- cancel-mid-stream save decision ---

// TestAskHandler_CanceledMidStream_SavesPartialAnswer covers the "decide
// whether to persist a partial answer on client disconnect" requirement:
// this project's chosen answer is YES when at least one token was streamed
// before the disconnect, with finish_reason "error" (it never legitimately
// completed).
func TestAskHandler_CanceledMidStream_SavesPartialAnswer(t *testing.T) {
	t.Parallel()
	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문서")}}
	ctx, cancel := context.WithCancel(context.Background())
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"부분", "답변"}, cancelAfter: 1, cancelFn: cancel}
	sessions := newFakeAskSessionStore()
	srv := newAskTestServerWithSessions(searcher, &fakeIntentClassifier{}, fakeLLM, sessions)

	doAskRequest(t, srv, ctx, map[string]any{"question": "질문"}, "Bearer test-key")

	if len(sessions.insertCalls) != 1 {
		t.Fatalf("Insert called %d times, want exactly 1 (partial answer must still be saved)", len(sessions.insertCalls))
	}
	saved := sessions.insertCalls[0]
	if saved.Answer != "부분" {
		t.Errorf("saved answer = %q, want the partial text streamed before cancellation %q", saved.Answer, "부분")
	}
	if saved.FinishReason != "error" {
		t.Errorf("saved finish_reason = %q, want %q (never legitimately completed)", saved.FinishReason, "error")
	}
}

// TestAskHandler_CanceledBeforeFirstToken_DoesNotSave: when the connection
// drops before any token was produced, there is no meaningful answer text
// to persist — no Insert call should happen at all.
func TestAskHandler_CanceledBeforeFirstToken_DoesNotSave(t *testing.T) {
	t.Parallel()
	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문서")}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before the handler even starts streaming
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"안 씀"}}
	sessions := newFakeAskSessionStore()
	srv := newAskTestServerWithSessions(searcher, &fakeIntentClassifier{}, fakeLLM, sessions)

	doAskRequest(t, srv, ctx, map[string]any{"question": "질문"}, "Bearer test-key")

	if len(sessions.insertCalls) != 0 {
		t.Errorf("Insert called %d times, want 0 (no token was ever produced)", len(sessions.insertCalls))
	}
}

// --- history cap ---

// TestRecentAskHistory_CapsAtMax asserts recentAskHistory keeps only the
// MOST RECENT askMaxHistoryTurns entries (oldest turns dropped first).
func TestRecentAskHistory_CapsAtMax(t *testing.T) {
	t.Parallel()
	var turns []store.AskSession
	for i := 0; i < askMaxHistoryTurns+3; i++ {
		turns = append(turns, store.AskSession{
			TurnIndex: i,
			Question:  fmtTurnLabel("Q", i),
			Answer:    fmtTurnLabel("A", i),
		})
	}

	history := recentAskHistory(turns, askMaxHistoryTurns)

	if len(history) != askMaxHistoryTurns {
		t.Fatalf("len(history) = %d, want %d", len(history), askMaxHistoryTurns)
	}
	// The oldest 3 turns (index 0,1,2) must have been dropped; the kept
	// window must start at turn index 3.
	if history[0].Question != fmtTurnLabel("Q", 3) {
		t.Errorf("history[0].Question = %q, want %q (oldest turns should be dropped first)", history[0].Question, fmtTurnLabel("Q", 3))
	}
	last := askMaxHistoryTurns + 2
	if history[len(history)-1].Question != fmtTurnLabel("Q", last) {
		t.Errorf("history[last].Question = %q, want %q", history[len(history)-1].Question, fmtTurnLabel("Q", last))
	}
}

func fmtTurnLabel(prefix string, i int) string {
	return prefix + "-" + strconv.Itoa(i)
}

// --- rewrite unit tests ---

// TestRewriteStandaloneQuestion_NoHistory_SkipsRewrite is the unit-level
// counterpart to TestAskHandler_FirstTurn_SkipsRewrite.
func TestRewriteStandaloneQuestion_NoHistory_SkipsRewrite(t *testing.T) {
	t.Parallel()
	fakeLLM := &fakeAskLLM{enabled: true, completeResp: "무시되어야 함"}
	srv := newAskTestServer(&fixedDocSearcher{}, &fakeIntentClassifier{}, fakeLLM)

	got, ok := srv.rewriteStandaloneQuestion(context.Background(), nil, "원래 질문")

	if ok {
		t.Error("ok = true, want false (no history to rewrite from)")
	}
	if got != "원래 질문" {
		t.Errorf("got = %q, want the original question unchanged", got)
	}
	if fakeLLM.completeCalls != 0 {
		t.Errorf("CompleteWithMessages called %d times, want 0", fakeLLM.completeCalls)
	}
}

// TestRewriteStandaloneQuestion_LLMError_FallsBackToOriginal asserts a
// rewrite-call error degrades to the original question rather than failing.
func TestRewriteStandaloneQuestion_LLMError_FallsBackToOriginal(t *testing.T) {
	t.Parallel()
	fakeLLM := &fakeAskLLM{enabled: true, completeErr: errors.New("llm: timeout")}
	srv := newAskTestServer(&fixedDocSearcher{}, &fakeIntentClassifier{}, fakeLLM)
	history := []askHistoryTurn{{Question: "이전 질문", Answer: "이전 답변"}}

	got, ok := srv.rewriteStandaloneQuestion(context.Background(), history, "원래 질문")

	if ok {
		t.Error("ok = true, want false on rewrite error")
	}
	if got != "원래 질문" {
		t.Errorf("got = %q, want the original question as fallback", got)
	}
}

// TestRewriteStandaloneQuestion_EmptyResponse_FallsBackToOriginal asserts
// an empty/whitespace-only rewrite response is treated as a failure, not a
// literal empty search query.
func TestRewriteStandaloneQuestion_EmptyResponse_FallsBackToOriginal(t *testing.T) {
	t.Parallel()
	fakeLLM := &fakeAskLLM{enabled: true, completeResp: "   "}
	srv := newAskTestServer(&fixedDocSearcher{}, &fakeIntentClassifier{}, fakeLLM)
	history := []askHistoryTurn{{Question: "이전 질문", Answer: "이전 답변"}}

	got, ok := srv.rewriteStandaloneQuestion(context.Background(), history, "원래 질문")

	if ok {
		t.Error("ok = true, want false on empty rewrite response")
	}
	if got != "원래 질문" {
		t.Errorf("got = %q, want the original question as fallback", got)
	}
}

// --- GET /api/v1/ask/conversations ---

func doAskGet(t *testing.T, srv *Server, path, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestAskConversationsHandler_ListsRecentConversations(t *testing.T) {
	t.Parallel()
	sessions := newFakeAskSessionStore()
	conv1, conv2 := uuid.New(), uuid.New()
	sessions.seed(conv1, store.AskSession{ID: uuid.New(), ConversationID: conv1, TurnIndex: 0, Question: "대화1", Answer: "답1", FinishReason: "stop"})
	sessions.seed(conv2, store.AskSession{ID: uuid.New(), ConversationID: conv2, TurnIndex: 0, Question: "대화2", Answer: "답2", FinishReason: "stop"})
	srv := newAskTestServerWithSessions(&fixedDocSearcher{}, &fakeIntentClassifier{}, &fakeAskLLM{enabled: true}, sessions)

	rr := doAskGet(t, srv, "/api/v1/ask/conversations", "Bearer test-key")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var out []askConversationSummary
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2", len(out))
	}
}

func TestAskConversationsHandler_RequiresAuth(t *testing.T) {
	t.Parallel()
	sessions := newFakeAskSessionStore()
	srv := newAskTestServerWithSessions(&fixedDocSearcher{}, &fakeIntentClassifier{}, &fakeAskLLM{enabled: true}, sessions)

	rr := doAskGet(t, srv, "/api/v1/ask/conversations", "")

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestAskConversationsRoutes_NotRegisteredWithoutSessions(t *testing.T) {
	t.Parallel()
	srv := newAskTestServer(&fixedDocSearcher{}, &fakeIntentClassifier{}, &fakeAskLLM{enabled: true}) // no WithAskSessions

	rr := doAskGet(t, srv, "/api/v1/ask/conversations", "Bearer test-key")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (route must not be registered without WithAskSessions)", rr.Code)
	}
}

func TestAskConversationDetailHandler_ReturnsTurnsInOrder(t *testing.T) {
	t.Parallel()
	sessions := newFakeAskSessionStore()
	conv := uuid.New()
	sessions.seed(conv,
		store.AskSession{ID: uuid.New(), ConversationID: conv, TurnIndex: 0, Question: "Q0", Answer: "A0", FinishReason: "stop"},
		store.AskSession{ID: uuid.New(), ConversationID: conv, TurnIndex: 1, Question: "Q1", Answer: "A1", FinishReason: "stop"},
	)
	srv := newAskTestServerWithSessions(&fixedDocSearcher{}, &fakeIntentClassifier{}, &fakeAskLLM{enabled: true}, sessions)

	rr := doAskGet(t, srv, "/api/v1/ask/conversations/"+conv.String(), "Bearer test-key")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var out []askConversationTurn
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2", len(out))
	}
	if out[0].Question != "Q0" || out[1].Question != "Q1" {
		t.Errorf("turns = %+v, want turn_index ASC order", out)
	}
}

func TestAskConversationDetailHandler_UnknownID_Returns404(t *testing.T) {
	t.Parallel()
	sessions := newFakeAskSessionStore()
	srv := newAskTestServerWithSessions(&fixedDocSearcher{}, &fakeIntentClassifier{}, &fakeAskLLM{enabled: true}, sessions)

	rr := doAskGet(t, srv, "/api/v1/ask/conversations/"+uuid.New().String(), "Bearer test-key")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rr.Code, rr.Body.String())
	}
}

func TestAskConversationDetailHandler_InvalidID_Returns400(t *testing.T) {
	t.Parallel()
	sessions := newFakeAskSessionStore()
	srv := newAskTestServerWithSessions(&fixedDocSearcher{}, &fakeIntentClassifier{}, &fakeAskLLM{enabled: true}, sessions)

	rr := doAskGet(t, srv, "/api/v1/ask/conversations/not-a-uuid", "Bearer test-key")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rr.Code, rr.Body.String())
	}
}

// --- occurred_at wire<->store round trip (issue #218) ---

// TestAskHandler_SaveAskTurn_PreservesOccurredAt covers the wire->store
// direction of saveAskTurn (ask_history.go): a source's OccurredAt, present
// in the SSE "sources" payload, must reach the persisted store.AskSession
// unchanged rather than being dropped during the AskSourceItem ->
// store.AskSource conversion.
func TestAskHandler_SaveAskTurn_PreservesOccurredAt(t *testing.T) {
	t.Parallel()
	occurredAt := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC)
	doc := docResultWithOccurredAt(model.SourceSMS, "문자 내용", occurredAt)
	searcher := &fixedDocSearcher{observed: []*model.SearchResult{doc}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"답"}}
	sessions := newFakeAskSessionStore()
	srv := newAskTestServerWithSessions(searcher, &fakeIntentClassifier{}, fakeLLM, sessions)

	rr := doAskRequest(t, srv, nil, map[string]any{"question": "질문"}, "Bearer test-key")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	if len(sessions.insertCalls) != 1 {
		t.Fatalf("Insert called %d times, want exactly 1", len(sessions.insertCalls))
	}
	saved := sessions.insertCalls[0]
	if len(saved.Sources) != 1 {
		t.Fatalf("saved.Sources = %v, want 1 item", saved.Sources)
	}
	got := saved.Sources[0].OccurredAt
	if got == nil {
		t.Fatalf("saved source OccurredAt = nil, want %s", occurredAt)
	}
	if !got.Equal(occurredAt) {
		t.Errorf("saved source OccurredAt = %s, want %s", got, occurredAt)
	}
}

// TestAskConversationDetailHandler_ReflectsSourceOccurredAt covers the
// store->wire direction (toAskSourceItems, ask_conversations.go): a
// pre-seeded store.AskSession with a set source OccurredAt must surface it
// unchanged through GET /api/v1/ask/conversations/{id}, and a source with no
// OccurredAt (mirroring a pre-#218 row that never had the key at all — see
// store.AskSource's doc comment) must surface as nil rather than the zero
// time.Time, so a page-refresh reload never fabricates a false event time
// for older turns.
func TestAskConversationDetailHandler_ReflectsSourceOccurredAt(t *testing.T) {
	t.Parallel()
	occurredAt := time.Date(2026, 8, 15, 12, 30, 0, 0, time.UTC)
	sessions := newFakeAskSessionStore()
	conv := uuid.New()
	sessions.seed(conv, store.AskSession{
		ID: uuid.New(), ConversationID: conv, TurnIndex: 0,
		Question: "Q0", Answer: "A0", FinishReason: "stop",
		Sources: []store.AskSource{
			{ID: "doc-with-time", Title: "t1", SourceType: "sms", Score: 0.9, OccurredAt: &occurredAt},
			{ID: "doc-without-time", Title: "t2", SourceType: "gmail", Score: 0.5}, // OccurredAt nil, as a pre-#218 row
		},
	})
	srv := newAskTestServerWithSessions(&fixedDocSearcher{}, &fakeIntentClassifier{}, &fakeAskLLM{enabled: true}, sessions)

	rr := doAskGet(t, srv, "/api/v1/ask/conversations/"+conv.String(), "Bearer test-key")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var out []askConversationTurn
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(out) != 1 || len(out[0].Sources) != 2 {
		t.Fatalf("out = %+v, want 1 turn with 2 sources", out)
	}
	if got := out[0].Sources[0].OccurredAt; got == nil || !got.Equal(occurredAt) {
		t.Errorf("sources[0].OccurredAt = %v, want %s", got, occurredAt)
	}
	if got := out[0].Sources[1].OccurredAt; got != nil {
		t.Errorf("sources[1].OccurredAt = %v, want nil (pre-#218 row has no captured event time)", got)
	}
}
