package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/timeutil"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// HTTP-contract-only tests for the golden-set handlers. The SQL contract
// (dedup, upsert idempotence, retention feedback, export shape) is pinned
// against a real PostgreSQL in internal/store/golden_db_test.go.
// ---------------------------------------------------------------------------

type stubGoldenSet struct {
	generateCreated, generateTotalOpen int
	generateErr                        error
	generateCalls                      int

	nextQuery    *store.GoldenQuery
	nextErr      error
	nextJudgeGot string

	judgedDocIDs   map[uuid.UUID]struct{}
	judgedErr      error
	judgedJudgeGot string

	progress    store.GoldenProgress
	progressErr error

	upsertQueryID   uuid.UUID
	upsertJudgments []store.GoldenJudgmentInput
	upsertFinish    bool
	upsertSaved     int
	upsertFeedback  int
	upsertErr       error

	skipQueryID uuid.UUID
	skipFound   bool
	skipErr     error

	exportPairs    []store.EvalPair
	exportErr      error
	exportJudgeGot string

	byTextID    uuid.UUID
	byTextText  string
	byTextFound bool
	byTextErr   error
}

func (s *stubGoldenSet) GenerateQueries(_ context.Context) (int, int, error) {
	s.generateCalls++
	return s.generateCreated, s.generateTotalOpen, s.generateErr
}

func (s *stubGoldenSet) NextQuery(_ context.Context, judge string) (*store.GoldenQuery, error) {
	s.nextJudgeGot = judge
	return s.nextQuery, s.nextErr
}

func (s *stubGoldenSet) JudgedDocumentIDs(_ context.Context, _ uuid.UUID, judge string) (map[uuid.UUID]struct{}, error) {
	s.judgedJudgeGot = judge
	return s.judgedDocIDs, s.judgedErr
}

func (s *stubGoldenSet) Progress(_ context.Context) (store.GoldenProgress, error) {
	return s.progress, s.progressErr
}

func (s *stubGoldenSet) UpsertJudgments(_ context.Context, queryID uuid.UUID, judgments []store.GoldenJudgmentInput, finish bool) (int, int, error) {
	s.upsertQueryID = queryID
	s.upsertJudgments = judgments
	s.upsertFinish = finish
	return s.upsertSaved, s.upsertFeedback, s.upsertErr
}

func (s *stubGoldenSet) SkipQuery(_ context.Context, id uuid.UUID) (bool, error) {
	s.skipQueryID = id
	return s.skipFound, s.skipErr
}

func (s *stubGoldenSet) ExportEvalPairs(_ context.Context, judge string) ([]store.EvalPair, error) {
	s.exportJudgeGot = judge
	return s.exportPairs, s.exportErr
}

func (s *stubGoldenSet) FindQueryByText(_ context.Context, text string) (uuid.UUID, bool, error) {
	s.byTextText = text
	return s.byTextID, s.byTextFound, s.byTextErr
}

// goldenStubSearcher is a search.DocumentSearcher fake returning the SAME
// canned results for every call, scoped to this file. goldenNextHandler
// issues two searches (relevance stream then recent stream); a stub that
// answers both identically is fine for tests that only care about one
// stream's output, since goldenMergeStreams' dedup absorbs the resulting
// overlap.
type goldenStubSearcher struct {
	results []*model.SearchResult
	err     error
}

func (g *goldenStubSearcher) Search(_ context.Context, _ model.SearchQuery) ([]*model.SearchResult, error) {
	return g.results, g.err
}

func newGoldenTestServer(g GoldenSet, searcher search.DocumentSearcher) *Server {
	if searcher == nil {
		searcher = &goldenStubSearcher{}
	}
	svc := search.NewService(searcher, askDisabledEmbedder{})
	return NewServer(nil, svc, nil, nil, nil, "", "").WithGolden(g)
}

func doGoldenRequest(srv *Server, method, path string, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestGoldenRoutes_NotRegisteredWhenNil pins the rollout mechanism: a Server
// built without WithGolden must not expose any /api/v1/golden/* route — the
// same "nil dependency means the route doesn't exist" contract every other
// optional route in router.go follows.
func TestGoldenRoutes_NotRegisteredWhenNil(t *testing.T) {
	t.Parallel()
	svc := search.NewService(&goldenStubSearcher{}, askDisabledEmbedder{})
	srv := NewServer(nil, svc, nil, nil, nil, "", "")

	for _, req := range []struct {
		method, path string
	}{
		{http.MethodPost, "/api/v1/golden/queries/generate"},
		{http.MethodGet, "/api/v1/golden/next"},
		{http.MethodPost, "/api/v1/golden/judgments"},
		{http.MethodPost, "/api/v1/golden/feedback"},
		{http.MethodPost, "/api/v1/golden/queries/11111111-2222-3333-4444-555555555555/skip"},
		{http.MethodGet, "/api/v1/golden/export"},
	} {
		rec := doGoldenRequest(srv, req.method, req.path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404 when golden is not wired", req.method, req.path, rec.Code)
		}
	}
}

func TestGoldenGenerateHandler_Success(t *testing.T) {
	t.Parallel()
	stub := &stubGoldenSet{generateCreated: 5, generateTotalOpen: 12}
	srv := newGoldenTestServer(stub, nil)

	rec := doGoldenRequest(srv, http.MethodPost, "/api/v1/golden/queries/generate", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var resp goldenGenerateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Created != 5 || resp.TotalOpen != 12 {
		t.Errorf("response = %+v, want created=5 total_open=12", resp)
	}
}

func TestGoldenGenerateHandler_StoreError(t *testing.T) {
	t.Parallel()
	stub := &stubGoldenSet{generateErr: errors.New("boom")}
	srv := newGoldenTestServer(stub, nil)

	rec := doGoldenRequest(srv, http.MethodPost, "/api/v1/golden/queries/generate", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// TestGoldenNextHandler_NoOpenQuery pins the "queue is empty" shape: query
// must be JSON null (not omitted, not an empty object) and candidates must
// be an empty array (not null) so a web client's .map() never needs a
// null-check.
func TestGoldenNextHandler_NoOpenQuery(t *testing.T) {
	t.Parallel()
	stub := &stubGoldenSet{progress: store.GoldenProgress{JudgedQueries: 3, OpenQueries: 0, TotalJudgments: 30}}
	srv := newGoldenTestServer(stub, nil)

	rec := doGoldenRequest(srv, http.MethodGet, "/api/v1/golden/next", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var resp goldenNextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Query != nil {
		t.Errorf("query = %+v, want nil when no open query remains", resp.Query)
	}
	if resp.Candidates == nil || len(resp.Candidates) != 0 {
		t.Errorf("candidates = %v, want an empty (non-nil) slice", resp.Candidates)
	}
	if resp.Progress.JudgedQueries != 3 || resp.Progress.TotalJudgments != 30 {
		t.Errorf("progress = %+v, want judged=3 total=30", resp.Progress)
	}
}

// TestGoldenNextHandler_ExcludesAlreadyJudged verifies that a candidate
// already judged for this query is filtered out and the remaining
// candidates are re-ranked from 1, and that IncludeRetention=true reaches
// BOTH search calls (disposable-tagged documents must still be judgeable on
// either stream).
//
// The query text ("지난주에 누구랑 통화했지") used to pin the "no explicit
// period" shape here (unconstrained relevance stream, 90-day-fallback recent
// stream, null query.window), because intent.DeterministicWindow did not
// recognise "지난주" yet. It now does (internal/intent/plan.go's lastWeekRe),
// so BOTH search streams must instead receive that resolved window — see
// TestGoldenNextHandler_NoWindow_WhenQueryHasNoPeriodPhrase below for the
// "no explicit period" shape this test used to (incidentally) also cover.
//
// storedAskedAt is set on the stub's GoldenQuery but deliberately far from
// reviewNow: the recent-stream fallback window and response asked_at must be
// computed from reviewNow (s.nowFunc(), injected below), NOT from this stored
// value — see goldenNextHandler's doc comment.
func TestGoldenNextHandler_ExcludesAlreadyJudged(t *testing.T) {
	t.Parallel()

	queryID := uuid.New()
	judgedDocID := uuid.New()
	freshDocID := uuid.New()
	occurredAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	storedAskedAt := time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC)
	reviewNow := time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)
	queryText := "지난주에 누구랑 통화했지"

	// Resolved at REVIEW TIME, not at storedAskedAt — same rule
	// TestGoldenNextHandler_PeriodPhraseResolvesWindowFromReviewTime pins.
	// reviewNow is 2026-09-20 (Sun) 12:00 KST, so its ISO week starts Monday
	// 2026-09-14; "지난주" is therefore the week before that.
	wantFrom, wantTo, label, ok := intent.DeterministicWindow(queryText, reviewNow.In(timeutil.KST()))
	if !ok {
		t.Fatalf("test setup: DeterministicWindow did not match %q", queryText)
	}
	if label != "지난 주" {
		t.Fatalf("test setup: label = %q, want %q", label, "지난 주")
	}

	stub := &stubGoldenSet{
		nextQuery: &store.GoldenQuery{ID: queryID, Text: queryText, Source: "seed", Status: "open", AskedAt: storedAskedAt},
		judgedDocIDs: map[uuid.UUID]struct{}{
			judgedDocID: {},
		},
		progress: store.GoldenProgress{OpenQueries: 1},
	}

	// Both streams answer with the SAME two documents: goldenMergeStreams'
	// dedup means the second (recent) stream's results are entirely absorbed
	// as duplicates of the first (relevance) stream's, leaving exactly one
	// surviving candidate tagged "relevance".
	sharedResults := []*model.SearchResult{
		{Document: model.Document{ID: judgedDocID, Title: "already judged", SourceType: model.SourceSMS}},
		{Document: model.Document{
			ID:         freshDocID,
			Title:      "fresh candidate",
			Content:    "line one\nline two   with   extra   spaces",
			SourceType: model.SourceCall,
			OccurredAt: &occurredAt,
			Metadata:   map[string]any{"retention": model.RetentionLow, "segment": "personal"},
		}},
	}
	searcher := &recordingGoldenSearcher{results: sharedResults}
	srv := newGoldenTestServer(stub, searcher)
	srv.now = func() time.Time { return reviewNow }

	rec := doGoldenRequest(srv, http.MethodGet, "/api/v1/golden/next?limit=5", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if len(searcher.calls) != 2 {
		t.Fatalf("Search called %d times, want 2 (relevance stream + recent stream)", len(searcher.calls))
	}
	relCall, recCall := searcher.calls[0], searcher.calls[1]
	if !relCall.IncludeRetention || !recCall.IncludeRetention {
		t.Error("IncludeRetention = false on some stream, want true on both (disposable docs must be judgeable)")
	}
	if relCall.Sort != "" {
		t.Errorf("relevance stream Sort = %q, want \"\" (score-ranked)", relCall.Sort)
	}
	if recCall.Sort != model.SortRecent {
		t.Errorf("recent stream Sort = %q, want %q", recCall.Sort, model.SortRecent)
	}
	if relCall.Limit != 3 { // round(5*0.6)
		t.Errorf("relevance stream Limit = %d, want 3 (round(5*0.6))", relCall.Limit)
	}
	if recCall.Limit != 2 { // round(5*0.4)
		t.Errorf("recent stream Limit = %d, want 2 (round(5*0.4))", recCall.Limit)
	}
	// "지난주" now resolves (internal/intent/plan.go's lastWeekRe), so BOTH
	// streams receive that SAME resolved window — not the unconstrained
	// relevance stream / 90-day recent fallback this test used to pin (see
	// TestGoldenNextHandler_NoWindow_WhenQueryHasNoPeriodPhrase for that
	// shape now).
	if relCall.OccurredFrom == nil || !relCall.OccurredFrom.Equal(wantFrom) {
		t.Errorf("relevance stream OccurredFrom = %v, want %v (resolved 지난주 window)", relCall.OccurredFrom, wantFrom)
	}
	if relCall.OccurredTo == nil || !relCall.OccurredTo.Equal(wantTo) {
		t.Errorf("relevance stream OccurredTo = %v, want %v", relCall.OccurredTo, wantTo)
	}
	if recCall.OccurredFrom == nil || !recCall.OccurredFrom.Equal(wantFrom) {
		t.Errorf("recent stream OccurredFrom = %v, want %v (same resolved window as the relevance stream, NOT the 90-day fallback)", recCall.OccurredFrom, wantFrom)
	}
	if recCall.OccurredTo == nil || !recCall.OccurredTo.Equal(wantTo) {
		t.Errorf("recent stream OccurredTo = %v, want %v", recCall.OccurredTo, wantTo)
	}
	if stub.nextJudgeGot != "user" {
		t.Errorf("NextQuery judge = %q, want default 'user' when ?judge= is omitted", stub.nextJudgeGot)
	}
	if stub.judgedJudgeGot != "user" {
		t.Errorf("JudgedDocumentIDs judge = %q, want default 'user' when ?judge= is omitted", stub.judgedJudgeGot)
	}

	var resp goldenNextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Query == nil || resp.Query.ID != queryID.String() {
		t.Fatalf("query = %+v, want id %s", resp.Query, queryID)
	}
	if resp.Query.AskedAt != reviewNow.Format(time.RFC3339) {
		t.Errorf("query.asked_at = %q, want reviewNow %q (NOT the stored asked_at %q)", resp.Query.AskedAt, reviewNow.Format(time.RFC3339), storedAskedAt.Format(time.RFC3339))
	}
	if resp.Query.Window == nil {
		t.Fatalf("query.window = %v, want a resolved 지난주 window", resp.Query.Window)
	}
	if resp.Query.Window.From != wantFrom.Format(time.RFC3339) {
		t.Errorf("query.window.from = %q, want %q", resp.Query.Window.From, wantFrom.Format(time.RFC3339))
	}
	if resp.Query.Window.To != wantTo.Format(time.RFC3339) {
		t.Errorf("query.window.to = %q, want %q", resp.Query.Window.To, wantTo.Format(time.RFC3339))
	}
	if len(resp.Candidates) != 1 {
		t.Fatalf("candidates = %v, want exactly 1 (already-judged doc excluded, recent-stream duplicate deduped)", resp.Candidates)
	}
	c := resp.Candidates[0]
	if c.DocumentID != freshDocID.String() {
		t.Errorf("candidate document_id = %s, want %s", c.DocumentID, freshDocID)
	}
	if c.Rank != 1 {
		t.Errorf("candidate rank = %d, want 1 (re-ranked after exclusion)", c.Rank)
	}
	if c.Stream != goldenStreamRelevance {
		t.Errorf("candidate stream = %q, want %q (relevance stream is merged first)", c.Stream, goldenStreamRelevance)
	}
	if c.Snippet != "line one line two with extra spaces" {
		t.Errorf("snippet = %q, want whitespace collapsed", c.Snippet)
	}
	if c.Retention != model.RetentionLow {
		t.Errorf("retention = %q, want %q", c.Retention, model.RetentionLow)
	}
	if c.Segment != "personal" {
		t.Errorf("segment = %q, want %q", c.Segment, "personal")
	}
	if c.OccurredAt == nil || !c.OccurredAt.Equal(occurredAt) {
		t.Errorf("occurred_at = %v, want %v", c.OccurredAt, occurredAt)
	}
}

// TestGoldenNextHandler_NoWindow_WhenQueryHasNoPeriodPhrase pins the shape
// TestGoldenNextHandler_ExcludesAlreadyJudged used to (incidentally) also
// cover before "지난주" became a recognised intent.DeterministicWindow phrase:
// a query with genuinely no period expression gets an unconstrained relevance
// stream, a recent stream falling back to the standard 90-day-before-reviewNow
// window, and a null query.window in the response.
func TestGoldenNextHandler_NoWindow_WhenQueryHasNoPeriodPhrase(t *testing.T) {
	t.Parallel()

	queryText := "누구랑 통화했지"
	reviewNow := time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)
	if _, _, _, ok := intent.DeterministicWindow(queryText, reviewNow.In(timeutil.KST())); ok {
		t.Fatalf("test setup: %q matched a DeterministicWindow phrase, want no match", queryText)
	}

	queryID := uuid.New()
	stub := &stubGoldenSet{
		nextQuery: &store.GoldenQuery{ID: queryID, Text: queryText, Source: "seed", Status: "open"},
		progress:  store.GoldenProgress{OpenQueries: 1},
	}
	searcher := &recordingGoldenSearcher{results: []*model.SearchResult{
		{Document: model.Document{ID: uuid.New(), Title: "a call", SourceType: model.SourceCall}},
	}}
	srv := newGoldenTestServer(stub, searcher)
	srv.now = func() time.Time { return reviewNow }

	rec := doGoldenRequest(srv, http.MethodGet, "/api/v1/golden/next", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if len(searcher.calls) != 2 {
		t.Fatalf("Search called %d times, want 2 (relevance stream + recent stream)", len(searcher.calls))
	}
	relCall, recCall := searcher.calls[0], searcher.calls[1]
	if relCall.OccurredFrom != nil || relCall.OccurredTo != nil {
		t.Errorf("relevance stream window = [%v, %v), want none (no period phrase in the query text)", relCall.OccurredFrom, relCall.OccurredTo)
	}
	wantRecentFrom := reviewNow.Add(-goldenRecentFallbackWindow)
	if recCall.OccurredFrom == nil || !recCall.OccurredFrom.Equal(wantRecentFrom) {
		t.Errorf("recent stream OccurredFrom = %v, want %v (reviewNow - 90d fallback)", recCall.OccurredFrom, wantRecentFrom)
	}
	if recCall.OccurredTo == nil || !recCall.OccurredTo.Equal(reviewNow) {
		t.Errorf("recent stream OccurredTo = %v, want reviewNow %v", recCall.OccurredTo, reviewNow)
	}

	var resp goldenNextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Query == nil {
		t.Fatalf("query = %v, want non-nil", resp.Query)
	}
	if resp.Query.Window != nil {
		t.Errorf("query.window = %+v, want nil (no period phrase in the query text)", resp.Query.Window)
	}
}

// TestGoldenNextHandler_PeriodPhraseResolvesWindowFromReviewTime covers a
// query text carrying an explicit period phrase ("오늘"): both search streams
// must receive the SAME window, resolved via intent.DeterministicWindow
// anchored at REVIEW TIME (s.nowFunc(), injected as reviewNow below) — NOT at
// the query's stored asked_at, which is deliberately set to a different date
// so a window computed off the stale stored value would fail obviously
// rather than by coincidence — and the response must surface that window,
// and its own asked_at field, as reviewNow.
func TestGoldenNextHandler_PeriodPhraseResolvesWindowFromReviewTime(t *testing.T) {
	t.Parallel()

	storedAskedAt := time.Date(2026, 5, 10, 1, 0, 0, 0, time.UTC) // 2026-05-10 10:00 KST
	reviewNow := time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC)     // 2026-09-20 10:00 KST
	wantFrom, wantTo, label, ok := intent.DeterministicWindow("오늘 통화 내역 보여줘", reviewNow.In(timeutil.KST()))
	if !ok {
		t.Fatalf("test setup: DeterministicWindow did not match %q", label)
	}
	// Sanity check that storedAskedAt would have resolved to a DIFFERENT
	// window — otherwise this test could pass even if the handler still read
	// q.AskedAt instead of reviewNow.
	if staleFrom, _, _, _ := intent.DeterministicWindow("오늘 통화 내역 보여줘", storedAskedAt.In(timeutil.KST())); staleFrom.Equal(wantFrom) {
		t.Fatalf("test setup: storedAskedAt and reviewNow resolve to the same window (%v) — pick dates far enough apart to distinguish them", wantFrom)
	}

	queryID := uuid.New()
	stub := &stubGoldenSet{
		nextQuery: &store.GoldenQuery{ID: queryID, Text: "오늘 통화 내역 보여줘", Source: "seed", Status: "open", AskedAt: storedAskedAt},
		progress:  store.GoldenProgress{OpenQueries: 1},
	}
	// One canned hit on every call so the merge is non-empty and the
	// window-relaxation fallback (TestGoldenNextHandler_WindowFallback*)
	// never kicks in here — this test's only concern is that both streams
	// receive the resolved window, not the fallback behavior.
	searcher := &recordingGoldenSearcher{results: []*model.SearchResult{
		{Document: model.Document{ID: uuid.New(), Title: "today's call", SourceType: model.SourceCall}},
	}}
	srv := newGoldenTestServer(stub, searcher)
	srv.now = func() time.Time { return reviewNow }

	rec := doGoldenRequest(srv, http.MethodGet, "/api/v1/golden/next", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if len(searcher.calls) != 2 {
		t.Fatalf("Search called %d times, want 2", len(searcher.calls))
	}
	for i, call := range searcher.calls {
		if call.OccurredFrom == nil || !call.OccurredFrom.Equal(wantFrom) {
			t.Errorf("call[%d].OccurredFrom = %v, want %v", i, call.OccurredFrom, wantFrom)
		}
		if call.OccurredTo == nil || !call.OccurredTo.Equal(wantTo) {
			t.Errorf("call[%d].OccurredTo = %v, want %v", i, call.OccurredTo, wantTo)
		}
	}

	var resp goldenNextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Query == nil || resp.Query.Window == nil {
		t.Fatalf("query.window = %v, want a resolved window", resp.Query)
	}
	if resp.Query.Window.From != wantFrom.Format(time.RFC3339) {
		t.Errorf("window.from = %q, want %q", resp.Query.Window.From, wantFrom.Format(time.RFC3339))
	}
	if resp.Query.Window.To != wantTo.Format(time.RFC3339) {
		t.Errorf("window.to = %q, want %q", resp.Query.Window.To, wantTo.Format(time.RFC3339))
	}
	if resp.Query.AskedAt != reviewNow.Format(time.RFC3339) {
		t.Errorf("query.asked_at = %q, want reviewNow %q", resp.Query.AskedAt, reviewNow.Format(time.RFC3339))
	}
}

// TestGoldenNextHandler_WindowIgnoresArbitrarilyOldStoredAskedAt is a direct,
// standalone assertion of the "질문 시점을 현재로" contract: no matter how far
// in the past a query's stored asked_at is (here, years old), the resolved
// window and the response's asked_at field must reflect review time
// (s.nowFunc()), never the stored value. The other window tests in this file
// pin this as a side effect of a more specific scenario; this test exists
// purely to make the general rule fail loudly on its own if it ever
// regresses.
func TestGoldenNextHandler_WindowIgnoresArbitrarilyOldStoredAskedAt(t *testing.T) {
	t.Parallel()

	ancientAskedAt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	reviewNow := time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC) // 2026-09-20 10:00 KST
	wantFrom, wantTo, label, ok := intent.DeterministicWindow("오늘 통화 내역 보여줘", reviewNow.In(timeutil.KST()))
	if !ok {
		t.Fatalf("test setup: DeterministicWindow did not match %q", label)
	}

	stub := &stubGoldenSet{
		nextQuery: &store.GoldenQuery{ID: uuid.New(), Text: "오늘 통화 내역 보여줘", Source: "seed", Status: "open", AskedAt: ancientAskedAt},
		progress:  store.GoldenProgress{OpenQueries: 1},
	}
	searcher := &recordingGoldenSearcher{results: []*model.SearchResult{
		{Document: model.Document{ID: uuid.New(), Title: "today's call", SourceType: model.SourceCall}},
	}}
	srv := newGoldenTestServer(stub, searcher)
	srv.now = func() time.Time { return reviewNow }

	rec := doGoldenRequest(srv, http.MethodGet, "/api/v1/golden/next", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if len(searcher.calls) != 2 {
		t.Fatalf("Search called %d times, want 2", len(searcher.calls))
	}
	for i, call := range searcher.calls {
		if call.OccurredFrom == nil || !call.OccurredFrom.Equal(wantFrom) {
			t.Errorf("call[%d].OccurredFrom = %v, want %v (reviewNow's window, NOT the 2020 stored asked_at's)", i, call.OccurredFrom, wantFrom)
		}
		if call.OccurredTo == nil || !call.OccurredTo.Equal(wantTo) {
			t.Errorf("call[%d].OccurredTo = %v, want %v (reviewNow's window, NOT the 2020 stored asked_at's)", i, call.OccurredTo, wantTo)
		}
	}

	var resp goldenNextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Query == nil || resp.Query.Window == nil {
		t.Fatalf("query.window = %v, want a resolved window", resp.Query)
	}
	if resp.Query.Window.From != wantFrom.Format(time.RFC3339) || resp.Query.Window.To != wantTo.Format(time.RFC3339) {
		t.Errorf("query.window = %+v, want %s..%s", resp.Query.Window, wantFrom, wantTo)
	}
	if resp.Query.AskedAt != reviewNow.Format(time.RFC3339) {
		t.Errorf("query.asked_at = %q, want reviewNow %q (NOT the stored 2020 asked_at)", resp.Query.AskedAt, reviewNow.Format(time.RFC3339))
	}
}

// TestGoldenNextHandler_MergesStreamsRatioAndDedup pins the merge itself: the
// relevance stream is exhausted before the recent stream contributes, a
// document present in both streams keeps its FIRST (relevance) stream label
// and is not double-counted, and the merged list is capped at the requested
// limit.
func TestGoldenNextHandler_MergesStreamsRatioAndDedup(t *testing.T) {
	t.Parallel()

	docA, docB, docC := uuid.New(), uuid.New(), uuid.New()
	stub := &stubGoldenSet{
		nextQuery: &store.GoldenQuery{ID: uuid.New(), Text: "프로젝트 진행 상황 어때", Source: "seed", Status: "open"},
		progress:  store.GoldenProgress{OpenQueries: 1},
	}
	searcher := &recordingGoldenSearcher{
		streamResults: [][]*model.SearchResult{
			{ // relevance stream
				{Document: model.Document{ID: docA, Title: "A", SourceType: model.SourceNote}},
				{Document: model.Document{ID: docB, Title: "B", SourceType: model.SourceNote}},
			},
			{ // recent stream — docB duplicates the relevance stream, docC is new
				{Document: model.Document{ID: docB, Title: "B", SourceType: model.SourceNote}},
				{Document: model.Document{ID: docC, Title: "C", SourceType: model.SourceNote}},
			},
		},
	}
	srv := newGoldenTestServer(stub, searcher)

	rec := doGoldenRequest(srv, http.MethodGet, "/api/v1/golden/next?limit=4", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if len(searcher.calls) != 2 {
		t.Fatalf("Search called %d times, want 2", len(searcher.calls))
	}
	if searcher.calls[0].Limit != 2 { // round(4*0.6) = 2 (banker's-adjacent .4*4=... actually round(2.4)=2)
		t.Errorf("relevance stream Limit = %d, want 2 (round(4*0.6))", searcher.calls[0].Limit)
	}
	if searcher.calls[1].Limit != 2 { // round(4*0.4) = 2 (round(1.6)=2)
		t.Errorf("recent stream Limit = %d, want 2 (round(4*0.4))", searcher.calls[1].Limit)
	}

	var resp goldenNextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Candidates) != 3 {
		t.Fatalf("candidates = %+v, want 3 (A, B, C — docB deduped once)", resp.Candidates)
	}
	wantOrder := []struct {
		id     uuid.UUID
		stream string
	}{
		{docA, goldenStreamRelevance},
		{docB, goldenStreamRelevance},
		{docC, goldenStreamRecent},
	}
	for i, want := range wantOrder {
		got := resp.Candidates[i]
		if got.DocumentID != want.id.String() {
			t.Errorf("candidates[%d].document_id = %s, want %s", i, got.DocumentID, want.id)
		}
		if got.Stream != want.stream {
			t.Errorf("candidates[%d].stream = %q, want %q", i, got.Stream, want.stream)
		}
		if got.Rank != i+1 {
			t.Errorf("candidates[%d].rank = %d, want %d", i, got.Rank, i+1)
		}
	}
}

// TestGoldenNextHandler_WindowFallbackWhenBothStreamsEmpty covers the
// production scenario that motivated the fallback: a query naming an
// explicit period ("오늘") whose window has no matching documents at all.
// goldenNextHandler must retry once with the window relaxed — relevance
// stream searched with NO window, recent stream falling back to the
// standard 90-day-before-reviewNow window — and report
// query.window_fallback=true while query.window still reflects the
// ORIGINALLY resolved (unhelpful) period, not the relaxed one. The query's
// stored asked_at is deliberately a different date from reviewNow, pinning
// that both the original window and the fallback window are anchored at
// review time (s.nowFunc()), not the stored value.
func TestGoldenNextHandler_WindowFallbackWhenBothStreamsEmpty(t *testing.T) {
	t.Parallel()

	storedAskedAt := time.Date(2026, 5, 10, 1, 0, 0, 0, time.UTC) // 2026-05-10 10:00 KST
	reviewNow := time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC)     // 2026-09-20 10:00 KST
	wantFrom, wantTo, label, ok := intent.DeterministicWindow("오늘 통화 내역 보여줘", reviewNow.In(timeutil.KST()))
	if !ok {
		t.Fatalf("test setup: DeterministicWindow did not match %q", label)
	}

	queryID := uuid.New()
	stub := &stubGoldenSet{
		nextQuery: &store.GoldenQuery{ID: queryID, Text: "오늘 통화 내역 보여줘", Source: "seed", Status: "open", AskedAt: storedAskedAt},
		progress:  store.GoldenProgress{OpenQueries: 1},
	}

	fallbackDocID := uuid.New()
	searcher := &recordingGoldenSearcher{
		streamResults: [][]*model.SearchResult{
			{}, // relevance stream, windowed to today: nothing
			{}, // recent stream, windowed to today: nothing
			{ // relevance fallback, no window: one hit
				{Document: model.Document{ID: fallbackDocID, Title: "fallback hit", SourceType: model.SourceCall}},
			},
			{}, // recent fallback, 90-day window: nothing new
		},
	}
	srv := newGoldenTestServer(stub, searcher)
	srv.now = func() time.Time { return reviewNow }

	rec := doGoldenRequest(srv, http.MethodGet, "/api/v1/golden/next", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if len(searcher.calls) != 4 {
		t.Fatalf("Search called %d times, want 4 (2 windowed + 2 fallback)", len(searcher.calls))
	}
	relCall, recCall, fbRelCall, fbRecCall := searcher.calls[0], searcher.calls[1], searcher.calls[2], searcher.calls[3]
	if relCall.OccurredFrom == nil || !relCall.OccurredFrom.Equal(wantFrom) {
		t.Errorf("relevance call OccurredFrom = %v, want %v (original window)", relCall.OccurredFrom, wantFrom)
	}
	if recCall.OccurredTo == nil || !recCall.OccurredTo.Equal(wantTo) {
		t.Errorf("recent call OccurredTo = %v, want %v (original window)", recCall.OccurredTo, wantTo)
	}
	if fbRelCall.OccurredFrom != nil || fbRelCall.OccurredTo != nil {
		t.Errorf("relevance fallback call window = [%v, %v), want none (whole corpus)", fbRelCall.OccurredFrom, fbRelCall.OccurredTo)
	}
	if fbRelCall.Sort != "" {
		t.Errorf("relevance fallback call Sort = %q, want \"\" (score-ranked)", fbRelCall.Sort)
	}
	wantFbRecentFrom := reviewNow.Add(-goldenRecentFallbackWindow)
	if fbRecCall.OccurredFrom == nil || !fbRecCall.OccurredFrom.Equal(wantFbRecentFrom) {
		t.Errorf("recent fallback call OccurredFrom = %v, want %v (reviewNow - 90d)", fbRecCall.OccurredFrom, wantFbRecentFrom)
	}
	if fbRecCall.OccurredTo == nil || !fbRecCall.OccurredTo.Equal(reviewNow) {
		t.Errorf("recent fallback call OccurredTo = %v, want reviewNow %v (NOT the stored asked_at %v)", fbRecCall.OccurredTo, reviewNow, storedAskedAt)
	}
	if fbRecCall.Sort != model.SortRecent {
		t.Errorf("recent fallback call Sort = %q, want %q", fbRecCall.Sort, model.SortRecent)
	}

	var resp goldenNextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Query == nil {
		t.Fatalf("query = nil, want the open query")
	}
	if !resp.Query.WindowFallback {
		t.Error("query.window_fallback = false, want true when both windowed streams return nothing")
	}
	if resp.Query.Window == nil || resp.Query.Window.From != wantFrom.Format(time.RFC3339) || resp.Query.Window.To != wantTo.Format(time.RFC3339) {
		t.Errorf("query.window = %+v, want the ORIGINAL resolved window %s..%s (fallback must not overwrite it)", resp.Query.Window, wantFrom, wantTo)
	}
	if len(resp.Candidates) != 1 || resp.Candidates[0].DocumentID != fallbackDocID.String() {
		t.Fatalf("candidates = %+v, want exactly the fallback hit %s", resp.Candidates, fallbackDocID)
	}
}

// TestGoldenNextHandler_NoWindowFallbackWhenPrimaryStreamsHaveResults covers
// the non-degenerate case: when the windowed streams already produce at
// least one candidate, goldenNextHandler must NOT retry with a relaxed
// window, and query.window_fallback must be omitted (false).
func TestGoldenNextHandler_NoWindowFallbackWhenPrimaryStreamsHaveResults(t *testing.T) {
	t.Parallel()

	askedAt := time.Date(2026, 5, 10, 1, 0, 0, 0, time.UTC)
	docID := uuid.New()
	stub := &stubGoldenSet{
		nextQuery: &store.GoldenQuery{ID: uuid.New(), Text: "오늘 통화 내역 보여줘", Source: "seed", Status: "open", AskedAt: askedAt},
		progress:  store.GoldenProgress{OpenQueries: 1},
	}
	searcher := &recordingGoldenSearcher{
		streamResults: [][]*model.SearchResult{
			{ // relevance stream: one hit, so no fallback is needed
				{Document: model.Document{ID: docID, Title: "today's call", SourceType: model.SourceCall}},
			},
			{}, // recent stream
		},
	}
	srv := newGoldenTestServer(stub, searcher)

	rec := doGoldenRequest(srv, http.MethodGet, "/api/v1/golden/next", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if len(searcher.calls) != 2 {
		t.Fatalf("Search called %d times, want 2 (no fallback retry expected)", len(searcher.calls))
	}

	var resp goldenNextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Query == nil {
		t.Fatalf("query = nil, want the open query")
	}
	if resp.Query.WindowFallback {
		t.Error("query.window_fallback = true, want false (omitted) when the primary streams already had a candidate")
	}
	if len(resp.Candidates) != 1 || resp.Candidates[0].DocumentID != docID.String() {
		t.Fatalf("candidates = %+v, want exactly %s", resp.Candidates, docID)
	}

	body := rec.Body.String()
	if strings.Contains(body, "window_fallback") {
		t.Errorf("response body contains window_fallback despite omitempty: %s", body)
	}
}

// recordingGoldenSearcher is a search.DocumentSearcher fake that records
// every model.SearchQuery it receives, scoped to this file.
// goldenNextHandler issues exactly two searches per request — a
// relevance-ranked one followed by a recent-ranked one — so tests read
// searcher.calls[0] / searcher.calls[1] to inspect each stream independently.
type recordingGoldenSearcher struct {
	// results is returned for every call when streamResults is nil.
	results []*model.SearchResult
	// streamResults, when non-nil, is indexed by call order: streamResults[0]
	// answers the first (relevance-stream) call, streamResults[1] the second
	// (recent-stream) call.
	streamResults [][]*model.SearchResult
	err           error
	calls         []model.SearchQuery
}

func (r *recordingGoldenSearcher) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	idx := len(r.calls)
	r.calls = append(r.calls, q)
	if r.err != nil {
		return nil, r.err
	}
	if r.streamResults != nil {
		if idx < len(r.streamResults) {
			return r.streamResults[idx], nil
		}
		return nil, nil
	}
	return r.results, nil
}

func TestGoldenJudgmentsHandler_Success(t *testing.T) {
	t.Parallel()
	stub := &stubGoldenSet{upsertSaved: 2, upsertFeedback: 1}
	srv := newGoldenTestServer(stub, nil)

	queryID := uuid.New()
	docID := uuid.New()
	body, _ := json.Marshal(map[string]any{
		"query_id": queryID.String(),
		"judgments": []map[string]any{
			{"document_id": docID.String(), "judgment": "relevant", "rank": 1},
		},
		"finish_query": true,
	})

	rec := doGoldenRequest(srv, http.MethodPost, "/api/v1/golden/judgments", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if stub.upsertQueryID != queryID {
		t.Errorf("upsertQueryID = %s, want %s", stub.upsertQueryID, queryID)
	}
	if !stub.upsertFinish {
		t.Error("upsertFinish = false, want true")
	}
	if len(stub.upsertJudgments) != 1 || stub.upsertJudgments[0].DocumentID != docID {
		t.Errorf("upsertJudgments = %+v, want one entry for %s", stub.upsertJudgments, docID)
	}

	var resp goldenJudgmentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Saved != 2 || resp.FeedbackApplied != 1 {
		t.Errorf("response = %+v, want saved=2 feedback_applied=1", resp)
	}
}

func TestGoldenJudgmentsHandler_InvalidJudgmentValue(t *testing.T) {
	t.Parallel()
	srv := newGoldenTestServer(&stubGoldenSet{}, nil)

	body, _ := json.Marshal(map[string]any{
		"query_id": uuid.New().String(),
		"judgments": []map[string]any{
			{"document_id": uuid.New().String(), "judgment": "spam"},
		},
	})
	rec := doGoldenRequest(srv, http.MethodPost, "/api/v1/golden/judgments", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unrecognised judgment value", rec.Code)
	}
}

func TestGoldenJudgmentsHandler_InvalidQueryID(t *testing.T) {
	t.Parallel()
	srv := newGoldenTestServer(&stubGoldenSet{}, nil)

	body, _ := json.Marshal(map[string]any{"query_id": "not-a-uuid", "judgments": []map[string]any{}})
	rec := doGoldenRequest(srv, http.MethodPost, "/api/v1/golden/judgments", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a non-UUID query_id", rec.Code)
	}
}

func TestGoldenSkipHandler_Success(t *testing.T) {
	t.Parallel()
	stub := &stubGoldenSet{skipFound: true}
	srv := newGoldenTestServer(stub, nil)

	id := uuid.New()
	rec := doGoldenRequest(srv, http.MethodPost, "/api/v1/golden/queries/"+id.String()+"/skip", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if stub.skipQueryID != id {
		t.Errorf("skipQueryID = %s, want %s", stub.skipQueryID, id)
	}
}

func TestGoldenSkipHandler_NotFound(t *testing.T) {
	t.Parallel()
	stub := &stubGoldenSet{skipFound: false}
	srv := newGoldenTestServer(stub, nil)

	rec := doGoldenRequest(srv, http.MethodPost, "/api/v1/golden/queries/"+uuid.New().String()+"/skip", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when the query is not open/does not exist", rec.Code)
	}
}

func TestGoldenExportHandler_Success(t *testing.T) {
	t.Parallel()
	stub := &stubGoldenSet{
		exportPairs: []store.EvalPair{
			{ID: 1, Query: "지난달 병원 예약", RelevantDocIDs: []string{uuid.NewString()}, Source: "golden"},
		},
	}
	srv := newGoldenTestServer(stub, nil)

	rec := doGoldenRequest(srv, http.MethodGet, "/api/v1/golden/export", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Eval-Pair-Count"); got != "1" {
		t.Errorf("X-Eval-Pair-Count = %q, want %q", got, "1")
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("body lines = %d, want 1; body = %s", len(lines), rec.Body.String())
	}
	var pair store.EvalPair
	if err := json.Unmarshal([]byte(lines[0]), &pair); err != nil {
		t.Fatalf("unmarshal exported pair: %v", err)
	}
	if pair.Query != "지난달 병원 예약" || pair.Source != "golden" {
		t.Errorf("pair = %+v, want query=%q source=golden", pair, "지난달 병원 예약")
	}
}

// TestGoldenFeedbackHandler_Success covers the hermes auto-eval entry point:
// the query is resolved by TEXT (not a pre-existing query_id), the source is
// used only for validation, and every judgment carries the
// request's judge value through to UpsertJudgments with finishQuery always
// false (a hermes conversation judging documents does not close out human
// review of that query).
func TestGoldenFeedbackHandler_Success(t *testing.T) {
	t.Parallel()
	stub := &stubGoldenSet{upsertSaved: 1, upsertFeedback: 1, byTextFound: true, byTextID: uuid.New()}
	srv := newGoldenTestServer(stub, nil)

	docID := uuid.New()
	body, _ := json.Marshal(map[string]any{
		"query_text": "지난주 통화 기록 보여줘",
		"source":     "hermes",
		"judge":      "user",
		"judgments": []map[string]any{
			{"document_id": docID.String(), "judgment": "relevant", "rank": 1},
		},
		"note": "hermes conversation snippet — must not be persisted or logged",
	})

	rec := doGoldenRequest(srv, http.MethodPost, "/api/v1/golden/feedback", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if stub.byTextText != "지난주 통화 기록 보여줘" {
		t.Errorf("FindQueryByText text = %q, want the request's query_text", stub.byTextText)
	}
	if stub.upsertQueryID != stub.byTextID {
		t.Errorf("UpsertJudgments queryID = %s, want the id FindQueryByText resolved (%s)", stub.upsertQueryID, stub.byTextID)
	}
	if stub.upsertFinish {
		t.Error("upsertFinish = true, want false — POST /golden/feedback must never close out the query")
	}
	if len(stub.upsertJudgments) != 1 || stub.upsertJudgments[0].Judge != "user" {
		t.Errorf("upsertJudgments = %+v, want one entry with Judge=user", stub.upsertJudgments)
	}

	var resp goldenFeedbackResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.QueryID != stub.byTextID.String() || resp.Saved != 1 || resp.FeedbackApplied != 1 {
		t.Errorf("response = %+v, want query_id=%s saved=1 feedback_applied=1", resp, stub.byTextID)
	}
}

// Legacy asked_at input remains accepted, but lookup-only feedback cannot
// rewrite the existing question's source or original timestamp.
func TestGoldenFeedbackHandler_AcceptsLegacyAskedAt(t *testing.T) {
	t.Parallel()
	stub := &stubGoldenSet{upsertSaved: 1, byTextFound: true, byTextID: uuid.New()}
	srv := newGoldenTestServer(stub, nil)

	wantAskedAt := time.Date(2026, 6, 15, 8, 30, 0, 0, time.UTC)
	body, _ := json.Marshal(map[string]any{
		"query_text": "이번 달 구독료 정리",
		"source":     "hermes",
		"judge":      "llm",
		"judgments":  []map[string]any{},
		"asked_at":   wantAskedAt.Format(time.RFC3339),
	})

	rec := doGoldenRequest(srv, http.MethodPost, "/api/v1/golden/feedback", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if stub.upsertQueryID != stub.byTextID {
		t.Fatal("feedback did not use the existing question")
	}
}

// TestGoldenFeedbackHandler_InvalidAskedAt covers a malformed asked_at value
// (not RFC3339): the handler must reject the request rather than silently
// falling back to "now" or passing a garbage timestamp to the store.
func TestGoldenFeedbackHandler_InvalidAskedAt(t *testing.T) {
	t.Parallel()
	srv := newGoldenTestServer(&stubGoldenSet{}, nil)

	body, _ := json.Marshal(map[string]any{
		"query_text": "질의",
		"source":     "hermes",
		"judge":      "llm",
		"judgments":  []map[string]any{},
		"asked_at":   "not-a-timestamp",
	})
	rec := doGoldenRequest(srv, http.MethodPost, "/api/v1/golden/feedback", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a non-RFC3339 asked_at", rec.Code)
	}
}

// TestGoldenFeedbackHandler_LLMJudgeDoesNotApplyRetentionTag covers the
// judge="llm" path: the handler must forward Judge="llm" on every judgment
// (which is what makes GoldenStore.UpsertJudgments skip the retention-tag
// write — see the real-database proof in
// TestGoldenStore_UpsertJudgments_LLMJudgeIsRecordedButNeverAppliesRetention).
// Here, at the HTTP layer, the observable contract is: the request's judge
// value reaches every store.GoldenJudgmentInput unchanged, and the handler
// itself applies no client-side stripping or reinterpretation of
// feedback_applied — it simply reports back whatever the store returns.
func TestGoldenFeedbackHandler_LLMJudgeDoesNotApplyRetentionTag(t *testing.T) {
	t.Parallel()
	// feedbackApplied=0 simulates what the real store does for an all-"llm"
	// batch (see UpsertJudgments' doc comment) — the stub does not
	// re-implement that rule, it only proves the handler passes it through.
	stub := &stubGoldenSet{upsertSaved: 2, upsertFeedback: 0, byTextFound: true, byTextID: uuid.New()}
	srv := newGoldenTestServer(stub, nil)

	docA, docB := uuid.New(), uuid.New()
	body, _ := json.Marshal(map[string]any{
		"query_text": "이번 주에 잡힌 회의 몇 개야",
		"source":     "hermes",
		"judge":      "llm",
		"judgments": []map[string]any{
			{"document_id": docA.String(), "judgment": "noise", "rank": 1},
			{"document_id": docB.String(), "judgment": "relevant", "rank": 2},
		},
	})

	rec := doGoldenRequest(srv, http.MethodPost, "/api/v1/golden/feedback", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if len(stub.upsertJudgments) != 2 {
		t.Fatalf("upsertJudgments = %+v, want 2 entries", stub.upsertJudgments)
	}
	for _, j := range stub.upsertJudgments {
		if j.Judge != "llm" {
			t.Errorf("judgment %+v: Judge = %q, want 'llm'", j, j.Judge)
		}
	}

	var resp goldenFeedbackResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.FeedbackApplied != 0 {
		t.Errorf("feedback_applied = %d, want 0 (judge='llm' must never apply retention feedback)", resp.FeedbackApplied)
	}
}

func TestGoldenFeedbackHandler_InvalidSource(t *testing.T) {
	t.Parallel()
	srv := newGoldenTestServer(&stubGoldenSet{}, nil)

	body, _ := json.Marshal(map[string]any{
		"query_text": "질의",
		"source":     "not-a-real-source",
		"judge":      "user",
		"judgments":  []map[string]any{},
	})
	rec := doGoldenRequest(srv, http.MethodPost, "/api/v1/golden/feedback", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unrecognised source", rec.Code)
	}
}

func TestGoldenFeedbackHandler_InvalidJudge(t *testing.T) {
	t.Parallel()
	srv := newGoldenTestServer(&stubGoldenSet{}, nil)

	body, _ := json.Marshal(map[string]any{
		"query_text": "질의",
		"source":     "hermes",
		"judge":      "robot",
		"judgments":  []map[string]any{},
	})
	rec := doGoldenRequest(srv, http.MethodPost, "/api/v1/golden/feedback", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unrecognised judge", rec.Code)
	}
}

func TestGoldenFeedbackHandlerMissingQuestionDoesNotGenerate(t *testing.T) {
	for _, judge := range []string{"user", "llm"} {
		t.Run(judge, func(t *testing.T) {
			stub := &stubGoldenSet{}
			body, _ := json.Marshal(map[string]any{"query_text": "새로운 질문", "source": "hermes", "judge": judge, "judgments": []any{}})
			rec := doGoldenRequest(newGoldenTestServer(stub, nil), http.MethodPost, "/api/v1/golden/feedback", body)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status=%d want404", rec.Code)
			}
			if stub.generateCalls != 0 || stub.upsertQueryID != uuid.Nil {
				t.Fatal("missing-question feedback wrote data")
			}
		})
	}
}

func TestGoldenReadAndReviewNeverGenerate(t *testing.T) {
	stub := &stubGoldenSet{skipFound: true}
	srv := newGoldenTestServer(stub, nil)
	for _, path := range []string{"/api/v1/golden/next", "/api/v1/golden/next?judge=llm", "/api/v1/golden/export"} {
		rec := doGoldenRequest(srv, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s=%d", path, rec.Code)
		}
	}
	id := uuid.New()
	body, _ := json.Marshal(map[string]any{"query_id": id.String(), "judgments": []any{}, "finish_query": true})
	if rec := doGoldenRequest(srv, http.MethodPost, "/api/v1/golden/judgments", body); rec.Code != http.StatusOK {
		t.Fatalf("judgments=%d", rec.Code)
	}
	if rec := doGoldenRequest(srv, http.MethodPost, "/api/v1/golden/queries/"+id.String()+"/skip", nil); rec.Code != http.StatusOK {
		t.Fatalf("skip=%d", rec.Code)
	}
	if stub.generateCalls != 0 || stub.byTextText != "" {
		t.Fatal("read/review path generated a question")
	}
	if rec := doGoldenRequest(srv, http.MethodGet, "/api/v1/golden/queries/generate", nil); rec.Code == http.StatusOK {
		t.Fatal("GET must not generate")
	}
	if stub.generateCalls != 0 {
		t.Fatal("GET generated questions")
	}
	if rec := doGoldenRequest(srv, http.MethodPost, "/api/v1/golden/queries/generate", nil); rec.Code != http.StatusOK {
		t.Fatalf("generate=%d", rec.Code)
	}
	if stub.generateCalls != 1 {
		t.Fatalf("explicit generation calls=%d", stub.generateCalls)
	}
}
