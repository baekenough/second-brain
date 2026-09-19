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

	byTextID      uuid.UUID
	byTextText    string
	byTextSource  string
	byTextAskedAt time.Time
	byTextErr     error
}

func (s *stubGoldenSet) GenerateQueries(_ context.Context) (int, int, error) {
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

func (s *stubGoldenSet) UpsertQueryByText(_ context.Context, text, source string, askedAt time.Time) (uuid.UUID, error) {
	s.byTextText = text
	s.byTextSource = source
	s.byTextAskedAt = askedAt
	if s.byTextErr != nil {
		return uuid.Nil, s.byTextErr
	}
	if s.byTextID == uuid.Nil {
		s.byTextID = uuid.New()
	}
	return s.byTextID, nil
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
// either stream). The query text ("지난주에 누구랑 통화했지") matches no
// intent.DeterministicWindow phrase, so this also pins the "no explicit
// period" shape: an unconstrained relevance stream plus a 90-day-fallback
// recent stream, and a null query.window in the response.
func TestGoldenNextHandler_ExcludesAlreadyJudged(t *testing.T) {
	t.Parallel()

	queryID := uuid.New()
	judgedDocID := uuid.New()
	freshDocID := uuid.New()
	occurredAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	askedAt := time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC)

	stub := &stubGoldenSet{
		nextQuery: &store.GoldenQuery{ID: queryID, Text: "지난주에 누구랑 통화했지", Source: "seed", Status: "open", AskedAt: askedAt},
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
			SourceType: model.SourceCallLog,
			OccurredAt: &occurredAt,
			Metadata:   map[string]any{"retention": model.RetentionLow, "segment": "personal"},
		}},
	}
	searcher := &recordingGoldenSearcher{results: sharedResults}
	srv := newGoldenTestServer(stub, searcher)

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
	if relCall.OccurredFrom != nil || relCall.OccurredTo != nil {
		t.Errorf("relevance stream window = [%v, %v), want none (no period phrase in the query text)", relCall.OccurredFrom, relCall.OccurredTo)
	}
	wantRecentFrom := askedAt.Add(-goldenRecentFallbackWindow)
	if recCall.OccurredFrom == nil || !recCall.OccurredFrom.Equal(wantRecentFrom) {
		t.Errorf("recent stream OccurredFrom = %v, want %v (asked_at - 90d fallback)", recCall.OccurredFrom, wantRecentFrom)
	}
	if recCall.OccurredTo == nil || !recCall.OccurredTo.Equal(askedAt) {
		t.Errorf("recent stream OccurredTo = %v, want asked_at %v", recCall.OccurredTo, askedAt)
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
	if resp.Query.AskedAt != askedAt.Format(time.RFC3339) {
		t.Errorf("query.asked_at = %q, want %q", resp.Query.AskedAt, askedAt.Format(time.RFC3339))
	}
	if resp.Query.Window != nil {
		t.Errorf("query.window = %+v, want nil (no period phrase in the query text)", resp.Query.Window)
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

// TestGoldenNextHandler_PeriodPhraseResolvesWindowFromAskedAt covers a query
// text carrying an explicit period phrase ("오늘"): both search streams must
// receive the SAME window, resolved via intent.DeterministicWindow anchored
// at the query's asked_at (NOT at whenever this test — or any real review —
// happens to run), and the response must surface that window verbatim.
func TestGoldenNextHandler_PeriodPhraseResolvesWindowFromAskedAt(t *testing.T) {
	t.Parallel()

	// Deliberately far from "now" so a window computed off time.Now() instead
	// of asked_at would fail obviously rather than by coincidence.
	askedAt := time.Date(2026, 5, 10, 1, 0, 0, 0, time.UTC) // 2026-05-10 10:00 KST
	wantFrom, wantTo, label, ok := intent.DeterministicWindow("오늘 통화 내역 보여줘", askedAt.In(timeutil.KST()))
	if !ok {
		t.Fatalf("test setup: DeterministicWindow did not match %q", label)
	}

	queryID := uuid.New()
	stub := &stubGoldenSet{
		nextQuery: &store.GoldenQuery{ID: queryID, Text: "오늘 통화 내역 보여줘", Source: "seed", Status: "open", AskedAt: askedAt},
		progress:  store.GoldenProgress{OpenQueries: 1},
	}
	searcher := &recordingGoldenSearcher{}
	srv := newGoldenTestServer(stub, searcher)

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
// forwarded to UpsertQueryByText unchanged, and every judgment carries the
// request's judge value through to UpsertJudgments with finishQuery always
// false (a hermes conversation judging documents does not close out human
// review of that query).
func TestGoldenFeedbackHandler_Success(t *testing.T) {
	t.Parallel()
	stub := &stubGoldenSet{upsertSaved: 1, upsertFeedback: 1}
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
		t.Errorf("UpsertQueryByText text = %q, want the request's query_text", stub.byTextText)
	}
	if stub.byTextSource != "hermes" {
		t.Errorf("UpsertQueryByText source = %q, want 'hermes'", stub.byTextSource)
	}
	if stub.upsertQueryID != stub.byTextID {
		t.Errorf("UpsertJudgments queryID = %s, want the id UpsertQueryByText resolved (%s)", stub.upsertQueryID, stub.byTextID)
	}
	if stub.upsertFinish {
		t.Error("upsertFinish = true, want false — POST /golden/feedback must never close out the query")
	}
	if len(stub.upsertJudgments) != 1 || stub.upsertJudgments[0].Judge != "user" {
		t.Errorf("upsertJudgments = %+v, want one entry with Judge=user", stub.upsertJudgments)
	}
	if !stub.byTextAskedAt.IsZero() {
		t.Errorf("UpsertQueryByText askedAt = %v, want the zero value (no asked_at in the request body means 'let the store default to now()')", stub.byTextAskedAt)
	}

	var resp goldenFeedbackResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.QueryID != stub.byTextID.String() || resp.Saved != 1 || resp.FeedbackApplied != 1 {
		t.Errorf("response = %+v, want query_id=%s saved=1 feedback_applied=1", resp, stub.byTextID)
	}
}

// TestGoldenFeedbackHandler_ForwardsAskedAt covers the optional asked_at
// field: when the hermes caller supplies one, it must reach
// GoldenStore.UpsertQueryByText exactly as given (RFC3339-parsed), not
// silently dropped or replaced with "now".
func TestGoldenFeedbackHandler_ForwardsAskedAt(t *testing.T) {
	t.Parallel()
	stub := &stubGoldenSet{upsertSaved: 1}
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
	if !stub.byTextAskedAt.Equal(wantAskedAt) {
		t.Errorf("UpsertQueryByText askedAt = %v, want %v", stub.byTextAskedAt, wantAskedAt)
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
	stub := &stubGoldenSet{upsertSaved: 2, upsertFeedback: 0}
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
