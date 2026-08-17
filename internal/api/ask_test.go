package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// --- test doubles ---

// fixedDocSearcher is a documentSearcher fake that returns a fixed Observed
// set for the 본검색 call (q.SourceType == nil) and a fixed Inferred set for
// the 인사이트검색 call (q.SourceType == &insight). When err is non-nil every
// call fails, mirroring assembleRetrieval's "propagate immediately" contract.
type fixedDocSearcher struct {
	observed []*model.SearchResult
	inferred []*model.SearchResult
	err      error

	// gotQueries records every SearchQuery.Query this fake received, in
	// call order — used by the query-rewrite tests to assert Stage 2 saw
	// the rewritten (standalone) question, not the user's raw follow-up
	// wording.
	gotQueries []string
}

func (f *fixedDocSearcher) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	f.gotQueries = append(f.gotQueries, q.Query)
	if f.err != nil {
		return nil, f.err
	}
	if q.SourceType != nil && *q.SourceType == model.SourceInsight {
		return f.inferred, nil
	}
	return f.observed, nil
}

// askDisabledEmbedder is a search.EmbeddingEngine that is always off, so
// search.Service.Search takes the full-text-only path and delegates
// straight to the fixedDocSearcher above (mirrors
// internal/search/insight_exclusion_test.go's disabledEmbedder).
type askDisabledEmbedder struct{}

func (askDisabledEmbedder) Embed(context.Context, string) ([]float32, error) { return nil, nil }
func (askDisabledEmbedder) EmbedBatch(context.Context, []string) ([][]float32, error) {
	return nil, nil
}
func (askDisabledEmbedder) Enabled() bool  { return false }
func (askDisabledEmbedder) Dimension() int { return 0 }

// fakeIntentClassifier returns a fixed Params regardless of the question,
// bypassing real regex/LLM classification so ask_test.go can drive Stage 2
// deterministically without needing the shared llmClient to double as a
// classification backend.
type fakeIntentClassifier struct {
	params intent.Params
	err    error
}

func (f *fakeIntentClassifier) Classify(_ context.Context, question string) (intent.Params, error) {
	p := f.params
	if p.RawQuery == "" {
		p.RawQuery = question
	}
	return p, f.err
}

// fakeAskLLM implements llm.Completer and llm.StreamCompleter. streamCalls /
// completeCalls record invocation counts so tests can assert "LLM never
// invoked" for the no_evidence path. When cancelFn is set, it is invoked
// right after the cancelAfter-th chunk is delivered, simulating a client
// disconnect mid-stream (the fake checks ctx.Done() before each subsequent
// chunk, mirroring how the real *llm.Client stops reading resp.Body once
// its context is cancelled).
type fakeAskLLM struct {
	enabled      bool
	chunks       []string
	streamErr    error
	completeResp string
	completeErr  error
	cancelAfter  int
	cancelFn     context.CancelFunc

	streamCalls   int
	completeCalls int

	// gotSystemPrompt / gotMessages record the last synthesis call's
	// arguments so tests can assert on buildAskSystemPrompt's injected-now
	// output and buildAskMessages' occurred_at rendering without duplicating
	// synthesize's plumbing (date-context fix, ask.go askSystemPrompt).
	gotSystemPrompt string
	gotMessages     []llm.Message

	// completeMessagesLog records every CompleteWithMessages call's
	// messages argument, in call order — unlike gotSystemPrompt/gotMessages
	// (which the streaming synthesis call for the SAME request overwrites
	// right after rewriteStandaloneQuestion runs), this lets rewrite tests
	// inspect exactly what was sent to the rewrite call specifically.
	completeMessagesLog [][]llm.Message
}

func (f *fakeAskLLM) Enabled() bool { return f.enabled }

func (f *fakeAskLLM) CompleteWithMessages(_ context.Context, systemPrompt string, messages []llm.Message) (string, error) {
	f.completeCalls++
	f.gotSystemPrompt = systemPrompt
	f.gotMessages = messages
	f.completeMessagesLog = append(f.completeMessagesLog, messages)
	return f.completeResp, f.completeErr
}

func (f *fakeAskLLM) StreamWithMessages(ctx context.Context, systemPrompt string, messages []llm.Message, onDelta func(string)) error {
	f.streamCalls++
	f.gotSystemPrompt = systemPrompt
	f.gotMessages = messages
	if f.streamErr != nil {
		return f.streamErr
	}
	for i, c := range f.chunks {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		onDelta(c)
		if f.cancelFn != nil && i+1 == f.cancelAfter {
			f.cancelFn()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

var _ llm.StreamCompleter = (*fakeAskLLM)(nil)

// fakeAskSessionStore is an in-memory AskConversationStore fake — no real
// DB needed to test askHandler's persistence/history behaviour. insertErr /
// listErr, when non-nil, make every Insert / List* call fail respectively,
// exercising askHandler's "a storage failure must never affect the client
// response" contract.
type fakeAskSessionStore struct {
	mu        sync.Mutex
	turnsByID map[uuid.UUID][]store.AskSession
	insertErr error
	listErr   error

	insertCalls []store.AskSession
}

func newFakeAskSessionStore() *fakeAskSessionStore {
	return &fakeAskSessionStore{turnsByID: map[uuid.UUID][]store.AskSession{}}
}

// seed pre-populates one conversation's turn history, as if earlier
// requests had already been answered and saved.
func (f *fakeAskSessionStore) seed(conversationID uuid.UUID, turns ...store.AskSession) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turnsByID[conversationID] = append([]store.AskSession{}, turns...)
}

func (f *fakeAskSessionStore) Insert(_ context.Context, session store.AskSession) (store.AskSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.insertCalls = append(f.insertCalls, session)
	if f.insertErr != nil {
		return store.AskSession{}, f.insertErr
	}
	if session.ID == uuid.Nil {
		session.ID = uuid.New()
	}
	session.CreatedAt = time.Now()
	f.turnsByID[session.ConversationID] = append(f.turnsByID[session.ConversationID], session)
	return session, nil
}

func (f *fakeAskSessionStore) ListConversationTurns(_ context.Context, conversationID uuid.UUID) ([]store.AskSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]store.AskSession{}, f.turnsByID[conversationID]...), nil
}

func (f *fakeAskSessionStore) ListRecentConversations(_ context.Context, limit int) ([]store.AskSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var latest []store.AskSession
	for _, turns := range f.turnsByID {
		if len(turns) == 0 {
			continue
		}
		latest = append(latest, turns[len(turns)-1])
	}
	sort.Slice(latest, func(i, j int) bool { return latest[i].CreatedAt.After(latest[j].CreatedAt) })
	if limit > 0 && len(latest) > limit {
		latest = latest[:limit]
	}
	return latest, nil
}

// --- helpers ---

func docResult(sourceType model.SourceType, title string) *model.SearchResult {
	return &model.SearchResult{
		Document: model.Document{ID: uuid.New(), SourceType: sourceType, Title: title, Content: "본문"},
		Score:    0.9,
	}
}

// newAskTestServer wires a Server whose *search.Service is backed by fake
// in-memory searcher + a disabled embedder (no real DB), and whose
// intentClassifier field is swapped for a fake (unexported-field access is
// valid from a same-package _test.go file).
func newAskTestServer(searcher documentSearcher, classifier intent.Classifier, llmClient llm.Completer) *Server {
	svc := search.NewService(searcher, askDisabledEmbedder{})
	srv := NewServer(nil, svc, nil, nil, llmClient, "", "test-key")
	srv.intentClassifier = classifier
	return srv
}

// newAskTestServerWithNow is newAskTestServer plus an injected clock,
// mirroring intent.SetNow's role for LLMClassifier: it lets tests pin
// buildAskSystemPrompt's "current time" instead of depending on the real
// clock (date-context fix — see ask.go's askSystemPrompt doc comment for
// the production bug this covers).
func newAskTestServerWithNow(searcher documentSearcher, classifier intent.Classifier, llmClient llm.Completer, now func() time.Time) *Server {
	srv := newAskTestServer(searcher, classifier, llmClient)
	srv.now = now
	return srv
}

// newAskTestServerWithSessions is newAskTestServer plus conversation
// persistence wired via WithAskSessions — the multi-turn (query rewrite +
// turn history + save) tests use this instead of newAskTestServer, which
// deliberately leaves askSessions nil (single-turn only, matching every
// pre-existing ask_test.go test's behaviour unchanged).
func newAskTestServerWithSessions(searcher documentSearcher, classifier intent.Classifier, llmClient llm.Completer, sessions AskConversationStore) *Server {
	srv := newAskTestServer(searcher, classifier, llmClient)
	return srv.WithAskSessions(sessions)
}

func doAskRequest(t *testing.T, srv *Server, ctx context.Context, body any, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatalf("encode request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ask", &buf)
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

type sseFrame struct {
	event string
	data  string
}

// parseSSEFrames mirrors web/src/lib/sseEvents.ts's splitSSEBuffer parsing
// logic closely enough to assert event order/content in tests.
func parseSSEFrames(t *testing.T, body string) []sseFrame {
	t.Helper()
	var frames []sseFrame
	for _, part := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		var f sseFrame
		for _, line := range strings.Split(part, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				f.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				f.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		frames = append(frames, f)
	}
	return frames
}

func frameEvents(frames []sseFrame) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = f.event
	}
	return out
}

// --- tests ---

func TestAskHandler_EmptyQuestion_Returns400NoSSEHeaders(t *testing.T) {
	t.Parallel()
	srv := newAskTestServer(&fixedDocSearcher{}, &fakeIntentClassifier{}, &fakeAskLLM{enabled: true})

	rr := doAskRequest(t, srv, nil, map[string]any{"question": "   "}, "Bearer test-key")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, must not be text/event-stream for a rejected request", ct)
	}
}

func TestAskHandler_AuthRequired(t *testing.T) {
	t.Parallel()
	srv := newAskTestServer(&fixedDocSearcher{}, &fakeIntentClassifier{}, &fakeAskLLM{enabled: true})

	rr := doAskRequest(t, srv, nil, map[string]any{"question": "안녕"}, "")

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body: %s", rr.Code, rr.Body.String())
	}
}

func TestAskHandler_NoObservedResults_NoEvidenceNoLLMCall(t *testing.T) {
	t.Parallel()
	insightDoc := docResult(model.SourceInsight, "추론 결과")
	searcher := &fixedDocSearcher{observed: nil, inferred: []*model.SearchResult{insightDoc}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"안 씀"}}
	srv := newAskTestServer(searcher, &fakeIntentClassifier{}, fakeLLM)

	rr := doAskRequest(t, srv, nil, map[string]any{"question": "아무 문서도 없는 질문"}, "Bearer test-key")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	frames := parseSSEFrames(t, rr.Body.String())
	got := frameEvents(frames)
	want := []string{"conversation", "sources", "done"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("events[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	var done askDonePayload
	if err := json.Unmarshal([]byte(frames[2].data), &done); err != nil {
		t.Fatalf("unmarshal done payload: %v", err)
	}
	if done.FinishReason != "no_evidence" {
		t.Errorf("finish_reason = %q, want %q", done.FinishReason, "no_evidence")
	}
	if fakeLLM.streamCalls != 0 || fakeLLM.completeCalls != 0 {
		t.Errorf("LLM was invoked (stream=%d, complete=%d) for a 0-Observed-result question", fakeLLM.streamCalls, fakeLLM.completeCalls)
	}
}

func TestAskHandler_HappyPath_EventOrderAndSourceIDs(t *testing.T) {
	t.Parallel()
	smsDoc := docResult(model.SourceSMS, "문자 내용")
	insightDoc := docResult(model.SourceInsight, "추론 내용")
	searcher := &fixedDocSearcher{
		observed: []*model.SearchResult{smsDoc},
		inferred: []*model.SearchResult{insightDoc},
	}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"안녕", "하세요", "!"}}
	srv := newAskTestServer(searcher, &fakeIntentClassifier{}, fakeLLM)

	rr := doAskRequest(t, srv, nil, map[string]any{"question": "지난달 요약해줘"}, "Bearer test-key")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	if xb := rr.Header().Get("X-Accel-Buffering"); xb != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", xb)
	}

	frames := parseSSEFrames(t, rr.Body.String())
	got := frameEvents(frames)
	want := []string{"conversation", "sources", "token", "token", "token", "done"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v; body=%s", got, want, rr.Body.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("events[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	var sources askSourcesPayload
	if err := json.Unmarshal([]byte(frames[1].data), &sources); err != nil {
		t.Fatalf("unmarshal sources payload: %v; raw=%s", err, frames[1].data)
	}
	if len(sources.Sources) != 2 {
		t.Fatalf("sources = %v, want 2 items (1 observed + 1 inferred)", sources.Sources)
	}
	if sources.Sources[0].ID != smsDoc.Document.ID.String() {
		t.Errorf("sources[0].ID = %q, want %q (observed must come first, spec §5.3)", sources.Sources[0].ID, smsDoc.Document.ID.String())
	}
	if sources.Sources[1].ID != insightDoc.Document.ID.String() {
		t.Errorf("sources[1].ID = %q, want %q", sources.Sources[1].ID, insightDoc.Document.ID.String())
	}

	var doneP askDonePayload
	if err := json.Unmarshal([]byte(frames[5].data), &doneP); err != nil {
		t.Fatalf("unmarshal done payload: %v", err)
	}
	if doneP.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want %q", doneP.FinishReason, "stop")
	}
}

func TestAskHandler_LLMNotConfigured_EmitsErrorThenDoneError(t *testing.T) {
	t.Parallel()
	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문서")}}
	fakeLLM := &fakeAskLLM{enabled: false}
	srv := newAskTestServer(searcher, &fakeIntentClassifier{}, fakeLLM)

	rr := doAskRequest(t, srv, nil, map[string]any{"question": "질문"}, "Bearer test-key")

	frames := parseSSEFrames(t, rr.Body.String())
	got := frameEvents(frames)
	want := []string{"conversation", "sources", "error", "done"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("events[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	var doneP askDonePayload
	_ = json.Unmarshal([]byte(frames[3].data), &doneP)
	if doneP.FinishReason != "error" {
		t.Errorf("finish_reason = %q, want %q", doneP.FinishReason, "error")
	}
	if fakeLLM.streamCalls != 0 {
		t.Errorf("StreamWithMessages was called (%d times) despite Enabled()==false", fakeLLM.streamCalls)
	}
}

func TestAskHandler_RetrievalError_EmitsErrorDoneNoSources(t *testing.T) {
	t.Parallel()
	searcher := &fixedDocSearcher{err: errors.New("store unavailable")}
	fakeLLM := &fakeAskLLM{enabled: true}
	srv := newAskTestServer(searcher, &fakeIntentClassifier{}, fakeLLM)

	rr := doAskRequest(t, srv, nil, map[string]any{"question": "질문"}, "Bearer test-key")

	frames := parseSSEFrames(t, rr.Body.String())
	got := frameEvents(frames)
	want := []string{"conversation", "error", "done"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v (no sources event — search failed before sources could be computed)", got, want)
	}
	var doneP askDonePayload
	_ = json.Unmarshal([]byte(frames[2].data), &doneP)
	if doneP.FinishReason != "error" {
		t.Errorf("finish_reason = %q, want %q", doneP.FinishReason, "error")
	}
}

func TestAskHandler_ClientDisconnectMidStream_NoFurtherWrites(t *testing.T) {
	t.Parallel()
	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문서")}}
	ctx, cancel := context.WithCancel(context.Background())
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"안녕", "하세요", "!"}, cancelAfter: 1, cancelFn: cancel}
	srv := newAskTestServer(searcher, &fakeIntentClassifier{}, fakeLLM)

	rr := doAskRequest(t, srv, ctx, map[string]any{"question": "질문"}, "Bearer test-key")

	frames := parseSSEFrames(t, rr.Body.String())
	got := frameEvents(frames)
	// Exactly "conversation" + "sources" + the single token delivered
	// before cancellation was observed; no "done" (connection is gone) and
	// no further "token" frames.
	want := []string{"conversation", "sources", "token"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v (no writes after client disconnect)", got, want)
	}
}

// TestAskHandler_SSEFieldNames pins the wire contract byte-for-byte against
// web/src/lib/types.ts (AskSourceItem, AskStreamEvent) and
// web/src/lib/sseEvents.ts (parseAskEvent) — a silent json tag typo here
// would not fail a Go test on its own, but this test parses the raw JSON
// keys directly (not through AskSourceItem) so a rename is caught.
func TestAskHandler_SSEFieldNames(t *testing.T) {
	t.Parallel()
	doc := docResult(model.SourceSMS, "문자 내용")
	searcher := &fixedDocSearcher{observed: []*model.SearchResult{doc}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"답변"}}
	srv := newAskTestServer(searcher, &fakeIntentClassifier{}, fakeLLM)

	rr := doAskRequest(t, srv, nil, map[string]any{"question": "질문"}, "Bearer test-key")
	frames := parseSSEFrames(t, rr.Body.String())

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(frames[0].data), &raw); err != nil {
		t.Fatalf("conversation payload not valid JSON: %v", err)
	}
	for _, want := range []string{"conversation_id", "turn_index"} {
		if _, ok := raw[want]; !ok {
			t.Errorf("conversation payload missing key %q; raw=%s", want, frames[0].data)
		}
	}

	if err := json.Unmarshal([]byte(frames[1].data), &raw); err != nil {
		t.Fatalf("sources payload not valid JSON: %v", err)
	}
	if _, ok := raw["sources"]; !ok {
		t.Fatalf("sources payload missing top-level %q key; raw=%s", "sources", frames[1].data)
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw["sources"], &items); err != nil {
		t.Fatalf("sources[] not valid JSON: %v", err)
	}
	for _, want := range []string{"id", "title", "source_type", "score"} {
		if _, ok := items[0][want]; !ok {
			t.Errorf("AskSourceItem JSON is missing key %q (types.ts:122-127); got keys=%v", want, mapKeys(items[0]))
		}
	}

	if err := json.Unmarshal([]byte(frames[2].data), &raw); err != nil {
		t.Fatalf("token payload not valid JSON: %v", err)
	}
	if _, ok := raw["text"]; !ok {
		t.Errorf("token payload missing key %q", "text")
	}

	if err := json.Unmarshal([]byte(frames[3].data), &raw); err != nil {
		t.Fatalf("done payload not valid JSON: %v", err)
	}
	if _, ok := raw["finish_reason"]; !ok {
		t.Errorf("done payload missing key %q", "finish_reason")
	}
}

func mapKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// --- date-context fix tests (issue: "내일 일정" produced "현재 시점을 알 수 없다") ---

// TestBuildAskSystemPrompt_IncludesInjectedNowDateAndWeekday asserts the
// system prompt is no longer a compile-time constant blind to "now" — it
// must render the injected current date, its Korean weekday, and an
// explicit instruction to resolve relative expressions ("내일" etc.)
// against that date.
func TestBuildAskSystemPrompt_IncludesInjectedNowDateAndWeekday(t *testing.T) {
	t.Parallel()
	// 2026-08-18 03:00 UTC == 2026-08-18 12:00 KST (Tuesday/화).
	now := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC)

	prompt := buildAskSystemPrompt(now)

	if !strings.Contains(prompt, "2026-08-18") {
		t.Errorf("prompt = %q, want it to contain the injected current date 2026-08-18", prompt)
	}
	if !strings.Contains(prompt, "화") {
		t.Errorf("prompt = %q, want it to contain the Korean weekday 화 for 2026-08-18", prompt)
	}
	if !strings.Contains(prompt, "내일") {
		t.Errorf("prompt = %q, want an instruction referencing relative date terms like 내일", prompt)
	}
}

// TestBuildAskSystemPrompt_ConvertsToKST_NotServerLocal pins a UTC instant
// whose calendar date differs from its KST calendar date (2026-08-17 20:00
// UTC == 2026-08-18 05:00 KST) — the prompt must show the KST date, proving
// the conversion is explicit and not dependent on the server's local
// timezone (which may be UTC in a container).
func TestBuildAskSystemPrompt_ConvertsToKST_NotServerLocal(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 20, 0, 0, 0, time.UTC)

	prompt := buildAskSystemPrompt(now)

	if !strings.Contains(prompt, "2026-08-18") {
		t.Errorf("prompt = %q, want the KST date 2026-08-18 (not the UTC date 2026-08-17)", prompt)
	}
	if strings.Contains(prompt, "2026-08-17") {
		t.Errorf("prompt = %q, must not contain the UTC calendar date 2026-08-17", prompt)
	}
}

// TestAskHandler_SystemPromptReflectsInjectedNow is the end-to-end version:
// it drives the full askHandler and asserts the system prompt actually
// passed to the LLM client (not just buildAskSystemPrompt in isolation)
// reflects the Server's injected clock.
func TestAskHandler_SystemPromptReflectsInjectedNow(t *testing.T) {
	t.Parallel()
	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문서")}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"답"}}
	fixedNow := time.Date(2026, 8, 18, 3, 0, 0, 0, time.UTC) // 2026-08-18 12:00 KST
	srv := newAskTestServerWithNow(searcher, &fakeIntentClassifier{}, fakeLLM, func() time.Time { return fixedNow })

	rr := doAskRequest(t, srv, nil, map[string]any{"question": "내일 일정 알려줘"}, "Bearer test-key")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(fakeLLM.gotSystemPrompt, "2026-08-18") {
		t.Errorf("system prompt = %q, want it to contain the injected current date 2026-08-18", fakeLLM.gotSystemPrompt)
	}
}

// TestBuildAskMessages_IncludesOccurredAt asserts each document context
// line carries its occurred_at timestamp (converted to KST) so the model
// can reason about when the document's content actually happened, not just
// what it says.
func TestBuildAskMessages_IncludesOccurredAt(t *testing.T) {
	t.Parallel()
	occurred := time.Date(2026, 6, 21, 5, 30, 0, 0, time.UTC) // 2026-06-21 14:30 KST
	doc := model.Document{ID: uuid.New(), SourceType: model.SourceSMS, Title: "문자", Content: "내용", OccurredAt: &occurred}
	result := RetrievalResult{Observed: []*model.SearchResult{{Document: doc, Score: 0.9}}}

	messages := buildAskMessages("질문", result, nil)

	if len(messages) == 0 {
		t.Fatal("buildAskMessages returned no messages")
	}
	contextMsg := messages[0].Content
	if !strings.Contains(contextMsg, "2026-06-21") || !strings.Contains(contextMsg, "14:30") {
		t.Errorf("context message = %q, want it to contain occurred_at converted to KST (2026-06-21 14:30)", contextMsg)
	}
}

// TestBuildAskMessages_NilOccurredAt_NoPanicAndLabelled covers the
// llm-memory-source case where OccurredAt is always null: buildAskMessages
// must not panic on a nil *time.Time, and should render an explicit
// "no timestamp" label rather than silently omitting the field (so the
// model doesn't misinterpret absence as "same day as now").
func TestBuildAskMessages_NilOccurredAt_NoPanicAndLabelled(t *testing.T) {
	t.Parallel()
	doc := model.Document{ID: uuid.New(), SourceType: model.SourceInsight, Title: "메모", Content: "내용", OccurredAt: nil}
	result := RetrievalResult{Observed: []*model.SearchResult{{Document: doc, Score: 0.9}}}

	var messages []llm.Message
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("buildAskMessages panicked on nil OccurredAt: %v", r)
			}
		}()
		messages = buildAskMessages("질문", result, nil)
	}()

	if len(messages) == 0 {
		t.Fatal("buildAskMessages returned no messages")
	}
	if !strings.Contains(messages[0].Content, "시각 정보 없음") {
		t.Errorf("context message = %q, want a null-occurred_at placeholder (\"시각 정보 없음\")", messages[0].Content)
	}
}
