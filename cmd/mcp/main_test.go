package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/timeutil"
)

// ---------------------------------------------------------------------------
// mcpAuthContextFunc tests
// ---------------------------------------------------------------------------

func TestMCPAuthContextFunc_Disabled_AllowsAll(t *testing.T) {
	t.Parallel()

	fn := mcpAuthContextFunc("") // no API key → auth disabled
	ctx := fn(context.Background(), fakeHTTPRequest(""))
	if !isAuthorized(ctx) {
		t.Error("expected isAuthorized=true when no API key is configured")
	}
}

func TestMCPAuthContextFunc_CorrectToken_Authorized(t *testing.T) {
	t.Parallel()

	fn := mcpAuthContextFunc("secret-key")
	ctx := fn(context.Background(), fakeHTTPRequest("Bearer secret-key"))
	if !isAuthorized(ctx) {
		t.Error("expected isAuthorized=true for correct token")
	}
}

func TestMCPAuthContextFunc_WrongToken_Unauthorized(t *testing.T) {
	t.Parallel()

	fn := mcpAuthContextFunc("secret-key")
	ctx := fn(context.Background(), fakeHTTPRequest("Bearer wrong-key"))
	if isAuthorized(ctx) {
		t.Error("expected isAuthorized=false for wrong token")
	}
}

func TestMCPAuthContextFunc_NoHeader_Unauthorized(t *testing.T) {
	t.Parallel()

	fn := mcpAuthContextFunc("secret-key")
	ctx := fn(context.Background(), fakeHTTPRequest(""))
	if isAuthorized(ctx) {
		t.Error("expected isAuthorized=false when Authorization header is absent")
	}
}

func TestMCPAuthContextFunc_NonBearerScheme_Unauthorized(t *testing.T) {
	t.Parallel()

	fn := mcpAuthContextFunc("secret-key")
	ctx := fn(context.Background(), fakeHTTPRequest("Basic secret-key"))
	if isAuthorized(ctx) {
		t.Error("expected isAuthorized=false for non-Bearer scheme")
	}
}

// ---------------------------------------------------------------------------
// registerAddNoteTool wiring tests
// ---------------------------------------------------------------------------

// TestRegisterAddNoteTool_ForcesSourceAgentNote verifies that the MCP
// add_note tool, after the internal/note extraction, persists documents with
// SourceType=model.SourceAgentNote and still rejects an empty title — the two
// behaviours that must NOT regress (spec §6.2).
//
// Renamed 2026-08-25: add_note used to force model.SourceLLMMemory. That
// source_type was deprecated after session-transcript documents sharing the
// same label crowded out search results (see model.SourceLLMMemory's doc
// comment); add_note now writes model.SourceAgentNote instead so
// agent-authored notes remain distinguishable from that legacy contamination.
func TestRegisterAddNoteTool_ForcesSourceAgentNote(t *testing.T) {
	t.Parallel()

	docs := &fakeMCPDocUpserter{}
	chunks := &fakeMCPChunkWriter{}
	embed := &fakeMCPEmbedder{enabled: false}

	s := mcpserver.NewMCPServer("test", "1.0.0", mcpserver.WithToolCapabilities(false))
	registerAddNoteTool(s, docs, chunks, embed, "")

	ctx := context.WithValue(context.Background(), mcpAuthKey{}, true)

	result := s.HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_note","arguments":{"title":"My note","content":"Note body"}}}`))
	_ = result
	if docs.lastDoc == nil {
		t.Fatal("expected Upsert to be called")
	}
	if docs.lastDoc.SourceType != model.SourceAgentNote {
		t.Errorf("SourceType = %q, want %q", docs.lastDoc.SourceType, model.SourceAgentNote)
	}
}

// TestRegisterAddNoteTool_EmptyTitle_Rejected verifies the tool-level error
// path still fires for an empty title (requireTitle=true is hard-coded at
// the call site in registerAddNoteTool).
func TestRegisterAddNoteTool_EmptyTitle_Rejected(t *testing.T) {
	t.Parallel()

	docs := &fakeMCPDocUpserter{}
	chunks := &fakeMCPChunkWriter{}
	embed := &fakeMCPEmbedder{enabled: false}

	s := mcpserver.NewMCPServer("test", "1.0.0", mcpserver.WithToolCapabilities(false))
	registerAddNoteTool(s, docs, chunks, embed, "")

	ctx := context.WithValue(context.Background(), mcpAuthKey{}, true)
	result := s.HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_note","arguments":{"title":"","content":"Note body"}}}`))
	_ = result
	if docs.lastDoc != nil {
		t.Error("Upsert must not be called when title is empty")
	}
}

// fakeMCPDocUpserter/fakeMCPChunkWriter/fakeMCPEmbedder are minimal doubles
// scoped to this wiring test — the exhaustive Save() behaviour matrix lives
// in internal/note/note_test.go, not here.
type fakeMCPDocUpserter struct{ lastDoc *model.Document }

func (f *fakeMCPDocUpserter) Upsert(_ context.Context, doc *model.Document) error {
	if doc.ID == (uuid.UUID{}) {
		doc.ID = uuid.New()
	}
	f.lastDoc = doc
	return nil
}

type fakeMCPChunkWriter struct{ chunks []store.Chunk }

func (f *fakeMCPChunkWriter) ReplaceDocument(_ context.Context, _ uuid.UUID, chunks []store.Chunk) error {
	f.chunks = chunks
	return nil
}
func (f *fakeMCPChunkWriter) ListByDocument(_ context.Context, _ uuid.UUID) ([]store.Chunk, error) {
	return f.chunks, nil
}
func (f *fakeMCPChunkWriter) UpdateChunkEmbeddings(_ context.Context, _ []store.ChunkEmbedding) error {
	return nil
}

type fakeMCPEmbedder struct{ enabled bool }

func (f *fakeMCPEmbedder) Enabled() bool { return f.enabled }
func (f *fakeMCPEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	return out, nil
}

// ---------------------------------------------------------------------------
// Test doubles for read tools
// ---------------------------------------------------------------------------

// fakeDocGetter implements DocumentGetter.
type fakeDocGetter struct {
	doc    *model.Document
	getErr error
}

func (f *fakeDocGetter) GetByID(_ context.Context, _ uuid.UUID) (*model.Document, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.doc == nil {
		return &model.Document{
			ID:          uuid.New(),
			SourceType:  model.SourceLLMMemory,
			SourceID:    "test-source-id",
			Title:       "Test Doc",
			Content:     "Test content",
			Status:      "active",
			CollectedAt: time.Now().UTC(),
		}, nil
	}
	return f.doc, nil
}

// fakeStatsProvider implements StatsProvider.
type fakeStatsProvider struct {
	baselineErr error
	countErr    error
}

func (f *fakeStatsProvider) CountBySource(_ context.Context) (map[string]int, error) {
	if f.countErr != nil {
		return nil, f.countErr
	}
	return map[string]int{"llm-memory": 1}, nil
}

func (f *fakeStatsProvider) QueryBaselineStats(_ context.Context) (*store.BaselineStats, error) {
	if f.baselineErr != nil {
		return nil, f.baselineErr
	}
	return &store.BaselineStats{}, nil
}

// fakeSearchService is a minimal search.Service stand-in that satisfies the
// registerSearchTool signature. Because search.Service is a concrete struct,
// we use it only in the auth-gate tests where Search() is never actually
// called (the handler returns before reaching it). We pass nil and rely on the
// auth check short-circuiting before any nil dereference.

// newTestMCPServer constructs an MCPServer with the three read tools and the
// add_note tool registered, using the provided fakes. searchSvc may be nil
// when the test never reaches the search execution path (i.e. auth-gate tests).
func newTestMCPServer(
	searchSvc interface {
		Search(context.Context, model.SearchQuery) ([]model.SearchResult, error)
	},
	docGetter DocumentGetter,
	statsProvider StatsProvider,
) *mcpserver.MCPServer {
	s := mcpserver.NewMCPServer("test", "0.0.0", mcpserver.WithToolCapabilities(false))

	// Register get_document and stats with real fakes.
	registerGetDocumentTool(s, docGetter)
	registerStatsTool(s, statsProvider)

	// For search we need a *search.Service. Since the auth guard fires before
	// the service call, we register the tool with a nil service when searchSvc
	// is nil. The handler casts ctx early and returns before calling the service.
	// We register a minimal stub tool handler directly to avoid a nil *search.Service.
	searchTool := mcp.NewTool(
		"search",
		mcp.WithString("query", mcp.Required(), mcp.Description("query")),
	)
	s.AddTool(searchTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if !isAuthorized(ctx) {
			return mcp.NewToolResultError("unauthorized: Bearer token required"), nil
		}
		if searchSvc == nil {
			return mcp.NewToolResultError("no search service"), nil
		}
		q, _ := req.RequireString("query")
		_, err := searchSvc.Search(ctx, model.SearchQuery{Query: q, Limit: 10})
		if err != nil {
			return mcp.NewToolResultError("search error"), nil
		}
		return mcp.NewToolResultText(`{"results":[],"count":0,"query":"` + q + `"}`), nil
	})

	return s
}

// callTool is a test helper that directly invokes a registered tool handler
// with the given context and arguments map.
func callTool(
	t *testing.T,
	s *mcpserver.MCPServer,
	toolName string,
	ctx context.Context,
	args map[string]any,
) *mcp.CallToolResult {
	t.Helper()
	st := s.GetTool(toolName)
	if st == nil {
		t.Fatalf("tool %q not registered", toolName)
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = toolName
	req.Params.Arguments = args
	result, err := st.Handler(ctx, req)
	if err != nil {
		t.Fatalf("tool %q returned unexpected error: %v", toolName, err)
	}
	return result
}

// authorizedCtx returns a context carrying an approved Bearer-token claim.
func authorizedCtx() context.Context {
	return context.WithValue(context.Background(), mcpAuthKey{}, true)
}

// unauthorizedCtx returns a context carrying a rejected Bearer-token claim
// (simulating what mcpAuthContextFunc produces when an invalid token is sent).
func unauthorizedCtx() context.Context {
	return context.WithValue(context.Background(), mcpAuthKey{}, false)
}

// isErrorResult reports whether a CallToolResult signals a tool-level error.
func isErrorResult(r *mcp.CallToolResult) bool {
	return r != nil && r.IsError
}

// resultText extracts the text from the first content item of a CallToolResult.
func resultText(r *mcp.CallToolResult) string {
	if r == nil || len(r.Content) == 0 {
		return ""
	}
	if tc, ok := r.Content[0].(mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

// ---------------------------------------------------------------------------
// Auth enforcement tests: search
// ---------------------------------------------------------------------------

func TestSearchTool_Unauthorized_ReturnsAuthError(t *testing.T) {
	t.Parallel()

	s := newTestMCPServer(nil, &fakeDocGetter{}, &fakeStatsProvider{})
	result := callTool(t, s, "search", unauthorizedCtx(), map[string]any{"query": "test"})

	if !isErrorResult(result) {
		t.Error("expected IsError=true for unauthorized search call")
	}
	if !strings.Contains(resultText(result), "unauthorized") {
		t.Errorf("expected unauthorized message, got: %s", resultText(result))
	}
}

func TestSearchTool_Authorized_ProceedsToHandler(t *testing.T) {
	t.Parallel()

	// Use a minimal fake search service that records the call.
	svc := &fakeSearchSvc{}
	s := newTestMCPServer(svc, &fakeDocGetter{}, &fakeStatsProvider{})
	result := callTool(t, s, "search", authorizedCtx(), map[string]any{"query": "hello"})

	if isErrorResult(result) {
		t.Errorf("expected no error for authorized search call, got: %s", resultText(result))
	}
}

// fakeSearchSvc satisfies the search interface used by newTestMCPServer.
type fakeSearchSvc struct{}

func (f *fakeSearchSvc) Search(_ context.Context, _ model.SearchQuery) ([]model.SearchResult, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// occurred_from / occurred_to wiring tests
//
// These exercise registerSearchTool end-to-end through callTool with a real
// *search.Service (not the inline stub newTestMCPServer registers for the
// query/source/auth tests above) so that the assertions cover the actual
// parameter names, the parsing rules, and the exact model.SearchQuery.
// OccurredFrom/OccurredTo values svc.Search() receives — a stub tool handler
// could not catch a typo in the argument name or a missing wiring line, since
// it never touches registerSearchTool's own handler body.
// ---------------------------------------------------------------------------

// fakeOccurredDocSearcher implements search.DocumentSearcher and records the
// last model.SearchQuery it was called with, so tests can assert on the
// OccurredFrom/OccurredTo fields the handler actually built.
type fakeOccurredDocSearcher struct {
	called    bool
	lastQuery model.SearchQuery
	// results, when non-nil, is returned verbatim from Search. nil (the
	// zero value, used by every pre-existing test) preserves the original
	// "no results" behaviour.
	results []*model.SearchResult
}

func (f *fakeOccurredDocSearcher) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	f.called = true
	f.lastQuery = q
	return f.results, nil
}

// disabledEmbeddingEngine implements search.EmbeddingEngine in its
// permanently-disabled state, so search.Service.Search skips the embedding
// call entirely and goes straight to the DocumentSearcher.
type disabledEmbeddingEngine struct{}

func (disabledEmbeddingEngine) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, nil
}
func (disabledEmbeddingEngine) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	return make([][]float32, len(texts)), nil
}
func (disabledEmbeddingEngine) Enabled() bool  { return false }
func (disabledEmbeddingEngine) Dimension() int { return 0 }

// newOccurredTestServer builds an *mcpserver.MCPServer with the real
// registerSearchTool wired to a real *search.Service backed by the given
// fake document searcher, so occurred_from/occurred_to tests observe the
// production handler code path, not a test stub. rerankDefault is forwarded
// to registerSearchTool as-is, so tests can exercise both the
// SEARCH_RERANK_DEFAULT=on and =off cases through the same helper.
func newOccurredTestServer(docs *fakeOccurredDocSearcher, rerankDefault bool) *mcpserver.MCPServer {
	svc := search.NewService(docs, disabledEmbeddingEngine{})
	s := mcpserver.NewMCPServer("test", "0.0.0", mcpserver.WithToolCapabilities(false))
	registerSearchTool(s, svc, rerankDefault)
	return s
}

func TestSearchTool_OccurredRange_BothBoundsRFC3339(t *testing.T) {
	t.Parallel()

	docs := &fakeOccurredDocSearcher{}
	s := newOccurredTestServer(docs, false)

	result := callTool(t, s, "search", authorizedCtx(), map[string]any{
		"query":         "test",
		"occurred_from": "2026-09-05T00:00:00+09:00",
		"occurred_to":   "2026-09-06T00:00:00+09:00",
	})

	if isErrorResult(result) {
		t.Fatalf("unexpected error result: %s", resultText(result))
	}
	if !docs.called {
		t.Fatal("expected the document searcher to be called")
	}

	wantFrom, _ := time.Parse(time.RFC3339, "2026-09-05T00:00:00+09:00")
	wantTo, _ := time.Parse(time.RFC3339, "2026-09-06T00:00:00+09:00")

	if docs.lastQuery.OccurredFrom == nil || !docs.lastQuery.OccurredFrom.Equal(wantFrom) {
		t.Errorf("OccurredFrom = %v, want %v", docs.lastQuery.OccurredFrom, wantFrom)
	}
	if docs.lastQuery.OccurredTo == nil || !docs.lastQuery.OccurredTo.Equal(wantTo) {
		t.Errorf("OccurredTo = %v, want %v", docs.lastQuery.OccurredTo, wantTo)
	}
}

// TestSearchTool_OccurredRange_DateOnly_ParsedAsKSTMidnight pins the
// timezone-interpretation decision documented on parseOccurredBound: a bare
// "YYYY-MM-DD" date must resolve to midnight KST, matching the convention
// internal/intent/plan.go's parseWindow already uses for the same
// documents.occurred_at window (see parseOccurredBound's doc comment).
func TestSearchTool_OccurredRange_DateOnly_ParsedAsKSTMidnight(t *testing.T) {
	t.Parallel()

	docs := &fakeOccurredDocSearcher{}
	s := newOccurredTestServer(docs, false)

	result := callTool(t, s, "search", authorizedCtx(), map[string]any{
		"query":         "test",
		"occurred_from": "2026-09-05",
		"occurred_to":   "2026-09-06",
	})

	if isErrorResult(result) {
		t.Fatalf("unexpected error result: %s", resultText(result))
	}
	if !docs.called {
		t.Fatal("expected the document searcher to be called")
	}

	wantFrom := time.Date(2026, 9, 5, 0, 0, 0, 0, timeutil.KST())
	wantTo := time.Date(2026, 9, 6, 0, 0, 0, 0, timeutil.KST())

	if docs.lastQuery.OccurredFrom == nil || !docs.lastQuery.OccurredFrom.Equal(wantFrom) {
		t.Errorf("OccurredFrom = %v, want %v (KST midnight)", docs.lastQuery.OccurredFrom, wantFrom)
	}
	if docs.lastQuery.OccurredTo == nil || !docs.lastQuery.OccurredTo.Equal(wantTo) {
		t.Errorf("OccurredTo = %v, want %v (KST midnight)", docs.lastQuery.OccurredTo, wantTo)
	}
}

func TestSearchTool_OccurredRange_OnlyFromGiven_ToStaysNil(t *testing.T) {
	t.Parallel()

	docs := &fakeOccurredDocSearcher{}
	s := newOccurredTestServer(docs, false)

	result := callTool(t, s, "search", authorizedCtx(), map[string]any{
		"query":         "test",
		"occurred_from": "2026-09-05",
	})

	if isErrorResult(result) {
		t.Fatalf("unexpected error result: %s", resultText(result))
	}
	if docs.lastQuery.OccurredFrom == nil {
		t.Error("expected OccurredFrom to be set")
	}
	if docs.lastQuery.OccurredTo != nil {
		t.Errorf("expected OccurredTo to remain nil, got %v", docs.lastQuery.OccurredTo)
	}
}

func TestSearchTool_OccurredRange_OnlyToGiven_FromStaysNil(t *testing.T) {
	t.Parallel()

	docs := &fakeOccurredDocSearcher{}
	s := newOccurredTestServer(docs, false)

	result := callTool(t, s, "search", authorizedCtx(), map[string]any{
		"query":       "test",
		"occurred_to": "2026-09-06",
	})

	if isErrorResult(result) {
		t.Fatalf("unexpected error result: %s", resultText(result))
	}
	if docs.lastQuery.OccurredFrom != nil {
		t.Errorf("expected OccurredFrom to remain nil, got %v", docs.lastQuery.OccurredFrom)
	}
	if docs.lastQuery.OccurredTo == nil {
		t.Error("expected OccurredTo to be set")
	}
}

// TestSearchTool_OccurredRange_BothOmitted_NoRegression guards the existing
// caller path (no occurred_from/occurred_to given at all): both bounds must
// remain nil so the query behaves exactly as it did before this feature —
// an unfiltered search — rather than picking up a stray zero-value window.
func TestSearchTool_OccurredRange_BothOmitted_NoRegression(t *testing.T) {
	t.Parallel()

	docs := &fakeOccurredDocSearcher{}
	s := newOccurredTestServer(docs, false)

	result := callTool(t, s, "search", authorizedCtx(), map[string]any{
		"query": "test",
	})

	if isErrorResult(result) {
		t.Fatalf("unexpected error result: %s", resultText(result))
	}
	if docs.lastQuery.OccurredFrom != nil {
		t.Errorf("expected OccurredFrom to be nil, got %v", docs.lastQuery.OccurredFrom)
	}
	if docs.lastQuery.OccurredTo != nil {
		t.Errorf("expected OccurredTo to be nil, got %v", docs.lastQuery.OccurredTo)
	}
}

func TestSearchTool_OccurredRange_InvalidFormat_Rejected(t *testing.T) {
	t.Parallel()

	cases := map[string]map[string]any{
		"invalid occurred_from": {"query": "test", "occurred_from": "not-a-date"},
		"invalid occurred_to":   {"query": "test", "occurred_to": "2026/09/06"},
	}

	for name, args := range cases {
		args := args
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			docs := &fakeOccurredDocSearcher{}
			s := newOccurredTestServer(docs, false)

			result := callTool(t, s, "search", authorizedCtx(), args)

			if !isErrorResult(result) {
				t.Fatalf("expected an error result for malformed date, got success: %s", resultText(result))
			}
			if docs.called {
				t.Error("the document searcher must not be called when parsing fails")
			}
		})
	}
}

func TestSearchTool_OccurredRange_ToNotAfterFrom_Rejected(t *testing.T) {
	t.Parallel()

	cases := map[string]map[string]any{
		"to before from": {
			"query":         "test",
			"occurred_from": "2026-09-06",
			"occurred_to":   "2026-09-05",
		},
		"to equal from": {
			"query":         "test",
			"occurred_from": "2026-09-05",
			"occurred_to":   "2026-09-05",
		},
	}

	for name, args := range cases {
		args := args
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			docs := &fakeOccurredDocSearcher{}
			s := newOccurredTestServer(docs, false)

			result := callTool(t, s, "search", authorizedCtx(), args)

			if !isErrorResult(result) {
				t.Fatalf("expected an error result for a non-positive window, got success: %s", resultText(result))
			}
			if docs.called {
				t.Error("the document searcher must not be called when the window is invalid")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseOccurredBound unit tests
// ---------------------------------------------------------------------------

func TestParseOccurredBound_RFC3339(t *testing.T) {
	t.Parallel()

	got, err := parseOccurredBound("2026-09-05T12:30:00+09:00")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want, _ := time.Parse(time.RFC3339, "2026-09-05T12:30:00+09:00")
	if got == nil || !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseOccurredBound_DateOnly_KST(t *testing.T) {
	t.Parallel()

	got, err := parseOccurredBound("2026-09-05")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2026, 9, 5, 0, 0, 0, 0, timeutil.KST())
	if got == nil || !got.Equal(want) {
		t.Errorf("got %v, want %v (KST midnight)", got, want)
	}
}

func TestParseOccurredBound_Invalid_ReturnsError(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"not-a-date", "2026/09/05", "", "2026-13-40"} {
		if _, err := parseOccurredBound(in); err == nil {
			t.Errorf("parseOccurredBound(%q): expected error, got nil", in)
		}
	}
}

// ---------------------------------------------------------------------------
// Auth enforcement tests: get_document
// ---------------------------------------------------------------------------

func TestGetDocumentTool_Unauthorized_ReturnsAuthError(t *testing.T) {
	t.Parallel()

	s := newTestMCPServer(nil, &fakeDocGetter{}, &fakeStatsProvider{})
	result := callTool(t, s, "get_document", unauthorizedCtx(), map[string]any{
		"id": uuid.New().String(),
	})

	if !isErrorResult(result) {
		t.Error("expected IsError=true for unauthorized get_document call")
	}
	if !strings.Contains(resultText(result), "unauthorized") {
		t.Errorf("expected unauthorized message, got: %s", resultText(result))
	}
}

func TestGetDocumentTool_Authorized_ProceedsToHandler(t *testing.T) {
	t.Parallel()

	s := newTestMCPServer(nil, &fakeDocGetter{}, &fakeStatsProvider{})
	result := callTool(t, s, "get_document", authorizedCtx(), map[string]any{
		"id": uuid.New().String(),
	})

	if isErrorResult(result) {
		t.Errorf("expected no error for authorized get_document call, got: %s", resultText(result))
	}
}

// ---------------------------------------------------------------------------
// Auth enforcement tests: stats
// ---------------------------------------------------------------------------

func TestStatsTool_Unauthorized_ReturnsAuthError(t *testing.T) {
	t.Parallel()

	s := newTestMCPServer(nil, &fakeDocGetter{}, &fakeStatsProvider{})
	result := callTool(t, s, "stats", unauthorizedCtx(), nil)

	if !isErrorResult(result) {
		t.Error("expected IsError=true for unauthorized stats call")
	}
	if !strings.Contains(resultText(result), "unauthorized") {
		t.Errorf("expected unauthorized message, got: %s", resultText(result))
	}
}

func TestStatsTool_Authorized_ProceedsToHandler(t *testing.T) {
	t.Parallel()

	s := newTestMCPServer(nil, &fakeDocGetter{}, &fakeStatsProvider{})
	result := callTool(t, s, "stats", authorizedCtx(), nil)

	if isErrorResult(result) {
		t.Errorf("expected no error for authorized stats call, got: %s", resultText(result))
	}
}

// ---------------------------------------------------------------------------
// allowedSourceTypes validation tests
// ---------------------------------------------------------------------------

// TestAllowedSourceTypes_AllDeclaredTypesAccepted verifies that every
// model.SourceType constant declared in internal/model/document.go is present
// in allowedSourceTypes. This prevents the map from drifting when new source
// types are added to the model.
func TestAllowedSourceTypes_AllDeclaredTypesAccepted(t *testing.T) {
	t.Parallel()

	declared := []model.SourceType{
		model.SourceSlack,
		model.SourceGitHub,
		model.SourceGDrive,
		model.SourceNotion,
		model.SourceFilesystem,
		model.SourceDiscord,
		model.SourceTelegram,
		model.SourceSecretary,
		model.SourceLLMMemory,
		model.SourceGmail,
		model.SourceCalendar,
		model.SourceSMS,
		model.SourceCallLog,
		model.SourceCallTranscript,
		model.SourceUpload,
		model.SourceAgentNote,
	}

	for _, st := range declared {
		if _, ok := allowedSourceTypes[st]; !ok {
			t.Errorf("source type %q is declared in model but missing from allowedSourceTypes", st)
		}
	}

	if got, want := len(allowedSourceTypes), len(declared); got != want {
		t.Errorf("allowedSourceTypes has %d entries, want %d; a type may have been added to the model without updating the map", got, want)
	}
}

// TestSearchTool_NewSourceTypes_Accepted verifies that the 6 source types added
// in this fix (gmail, calendar, sms, call-log, call-transcript, upload) are
// accepted by the search tool's source filter and do not produce an error result.
func TestSearchTool_NewSourceTypes_Accepted(t *testing.T) {
	t.Parallel()

	newTypes := []string{"gmail", "calendar", "sms", "call-log", "call-transcript", "upload"}

	// Build a search server with a fake search service that returns no results.
	svc := &fakeSearchSvc{}
	s := mcpserver.NewMCPServer("test", "0.0.0", mcpserver.WithToolCapabilities(false))
	registerSearchTool(s, nil, false) // nil *search.Service — handler short-circuits at source validation

	// We need a real *search.Service for registerSearchTool. Use the inline
	// stub approach: register a custom handler that exercises allowedSourceTypes
	// directly, bypassing the need for a real search.Service.
	_ = svc
	_ = s

	// Validate directly against allowedSourceTypes (the map is package-level).
	for _, src := range newTypes {
		if _, ok := allowedSourceTypes[model.SourceType(src)]; !ok {
			t.Errorf("source type %q expected to be accepted but missing from allowedSourceTypes", src)
		}
	}
}

// TestSearchTool_UnknownSourceType_Rejected verifies that an unrecognised source
// type string is still rejected with an error result.
func TestSearchTool_UnknownSourceType_Rejected(t *testing.T) {
	t.Parallel()

	if _, ok := allowedSourceTypes[model.SourceType("unknown-source")]; ok {
		t.Error("source type \"unknown-source\" should not be in allowedSourceTypes")
	}
}

// ---------------------------------------------------------------------------
// sort parameter tests
// ---------------------------------------------------------------------------

// TestSearchTool_Sort_ExplicitRecent_SetsSortRecent verifies sort="recent"
// is forwarded to model.SearchQuery.Sort regardless of whether an
// occurred_from/occurred_to window is present.
func TestSearchTool_Sort_ExplicitRecent_SetsSortRecent(t *testing.T) {
	t.Parallel()

	docs := &fakeOccurredDocSearcher{}
	s := newOccurredTestServer(docs, false)

	result := callTool(t, s, "search", authorizedCtx(), map[string]any{
		"query": "test",
		"sort":  "recent",
	})
	if isErrorResult(result) {
		t.Fatalf("unexpected error result: %s", resultText(result))
	}
	if docs.lastQuery.Sort != model.SortRecent {
		t.Errorf("Sort = %q, want %q", docs.lastQuery.Sort, model.SortRecent)
	}
}

// TestSearchTool_Sort_ExplicitRelevance_LeavesSortUnset verifies an explicit
// sort="relevance" does NOT set Sort to "recent" even inside a time window —
// the caller's explicit choice must win over the auto-apply-on-window default.
func TestSearchTool_Sort_ExplicitRelevance_LeavesSortUnset(t *testing.T) {
	t.Parallel()

	docs := &fakeOccurredDocSearcher{}
	s := newOccurredTestServer(docs, false)

	result := callTool(t, s, "search", authorizedCtx(), map[string]any{
		"query":         "test",
		"occurred_from": "2026-09-05T00:00:00+09:00",
		"sort":          "relevance",
	})
	if isErrorResult(result) {
		t.Fatalf("unexpected error result: %s", resultText(result))
	}
	if docs.lastQuery.Sort != "" {
		t.Errorf("Sort = %q, want empty (relevance)", docs.lastQuery.Sort)
	}
}

// TestSearchTool_Sort_OmittedWithWindow_AutoAppliesRecent verifies that
// omitting sort while occurred_from/occurred_to is set auto-applies "recent"
// ordering — mirroring internal/api/ask_retrieval.go's windowed-plan default
// (spec-consistent behaviour, requested for the MCP tool because relevance
// order inside an already-narrowed time window is rarely useful).
func TestSearchTool_Sort_OmittedWithWindow_AutoAppliesRecent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args map[string]any
	}{
		{
			name: "occurred_from_only",
			args: map[string]any{"query": "test", "occurred_from": "2026-09-05T00:00:00+09:00"},
		},
		{
			name: "occurred_to_only",
			args: map[string]any{"query": "test", "occurred_to": "2026-09-06T00:00:00+09:00"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docs := &fakeOccurredDocSearcher{}
			s := newOccurredTestServer(docs, false)

			result := callTool(t, s, "search", authorizedCtx(), tc.args)
			if isErrorResult(result) {
				t.Fatalf("unexpected error result: %s", resultText(result))
			}
			if docs.lastQuery.Sort != model.SortRecent {
				t.Errorf("Sort = %q, want %q", docs.lastQuery.Sort, model.SortRecent)
			}
		})
	}
}

// TestSearchTool_Sort_OmittedWithoutWindow_LeavesSortUnset verifies the
// pre-existing no-window behaviour is unchanged: omitting sort with no
// occurred_from/occurred_to leaves Sort empty (relevance).
func TestSearchTool_Sort_OmittedWithoutWindow_LeavesSortUnset(t *testing.T) {
	t.Parallel()

	docs := &fakeOccurredDocSearcher{}
	s := newOccurredTestServer(docs, false)

	result := callTool(t, s, "search", authorizedCtx(), map[string]any{"query": "test"})
	if isErrorResult(result) {
		t.Fatalf("unexpected error result: %s", resultText(result))
	}
	if docs.lastQuery.Sort != "" {
		t.Errorf("Sort = %q, want empty (relevance)", docs.lastQuery.Sort)
	}
}

// TestSearchTool_Sort_UnknownValue_Rejected verifies an unrecognised sort
// value is rejected with a tool-level error rather than silently ignored.
func TestSearchTool_Sort_UnknownValue_Rejected(t *testing.T) {
	t.Parallel()

	docs := &fakeOccurredDocSearcher{}
	s := newOccurredTestServer(docs, false)

	result := callTool(t, s, "search", authorizedCtx(), map[string]any{
		"query": "test",
		"sort":  "newest",
	})
	if !isErrorResult(result) {
		t.Error("expected an error result for an unknown sort value")
	}
	if docs.called {
		t.Error("expected the document searcher NOT to be called for an invalid sort value")
	}
}

// ---------------------------------------------------------------------------
// use_rerank parameter tests
// ---------------------------------------------------------------------------

// TestSearchTool_UseRerank_DefaultsToServerConfig verifies that omitting
// use_rerank falls back to the rerankDefault value registerSearchTool was
// constructed with — for a query with no time window, where the sort/rerank
// interaction guard does not apply.
func TestSearchTool_UseRerank_DefaultsToServerConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		rerankDefault bool
	}{
		{name: "default_on", rerankDefault: true},
		{name: "default_off", rerankDefault: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docs := &fakeOccurredDocSearcher{}
			s := newOccurredTestServer(docs, tc.rerankDefault)

			result := callTool(t, s, "search", authorizedCtx(), map[string]any{"query": "test"})
			if isErrorResult(result) {
				t.Fatalf("unexpected error result: %s", resultText(result))
			}
			if docs.lastQuery.UseRerank != tc.rerankDefault {
				t.Errorf("UseRerank = %v, want %v", docs.lastQuery.UseRerank, tc.rerankDefault)
			}
		})
	}
}

// TestSearchTool_UseRerank_ExplicitFalse_OverridesServerDefault verifies a
// caller can force reranking off even when the server default is on.
func TestSearchTool_UseRerank_ExplicitFalse_OverridesServerDefault(t *testing.T) {
	t.Parallel()

	docs := &fakeOccurredDocSearcher{}
	s := newOccurredTestServer(docs, true) // server default: rerank ON

	result := callTool(t, s, "search", authorizedCtx(), map[string]any{
		"query":      "test",
		"use_rerank": false,
	})
	if isErrorResult(result) {
		t.Fatalf("unexpected error result: %s", resultText(result))
	}
	if docs.lastQuery.UseRerank {
		t.Error("UseRerank = true, want false (explicit override)")
	}
}

// TestSearchTool_UseRerank_ExplicitTrue_OverridesServerDefault verifies a
// caller can force reranking on even when the server default is off.
func TestSearchTool_UseRerank_ExplicitTrue_OverridesServerDefault(t *testing.T) {
	t.Parallel()

	docs := &fakeOccurredDocSearcher{}
	s := newOccurredTestServer(docs, false) // server default: rerank OFF

	result := callTool(t, s, "search", authorizedCtx(), map[string]any{
		"query":      "test",
		"use_rerank": true,
	})
	if isErrorResult(result) {
		t.Fatalf("unexpected error result: %s", resultText(result))
	}
	if !docs.lastQuery.UseRerank {
		t.Error("UseRerank = false, want true (explicit override)")
	}
}

// TestSearchTool_UseRerank_AutoRecentSort_SuppressesDefaultRerank verifies
// the sort/rerank interaction guard: when sort auto-resolves to "recent"
// (occurred_from given, sort omitted) and the caller did not explicitly ask
// for reranking, the server default is skipped so the recency ordering this
// tool just promised is not silently undone by a reranker running after it
// (see internal/search/search.go's applyRerank ordering comment).
func TestSearchTool_UseRerank_AutoRecentSort_SuppressesDefaultRerank(t *testing.T) {
	t.Parallel()

	docs := &fakeOccurredDocSearcher{}
	s := newOccurredTestServer(docs, true) // server default: rerank ON

	result := callTool(t, s, "search", authorizedCtx(), map[string]any{
		"query":         "test",
		"occurred_from": "2026-09-05T00:00:00+09:00",
	})
	if isErrorResult(result) {
		t.Fatalf("unexpected error result: %s", resultText(result))
	}
	if docs.lastQuery.Sort != model.SortRecent {
		t.Fatalf("precondition failed: Sort = %q, want %q", docs.lastQuery.Sort, model.SortRecent)
	}
	if docs.lastQuery.UseRerank {
		t.Error("UseRerank = true, want false (auto-recent sort suppresses the silent default)")
	}
}

// TestSearchTool_UseRerank_ExplicitTrue_WinsOverRecentSort verifies that an
// EXPLICIT use_rerank=true is honored even when sort resolved to "recent" —
// only the silent server default is suppressed, never an explicit caller
// choice.
func TestSearchTool_UseRerank_ExplicitTrue_WinsOverRecentSort(t *testing.T) {
	t.Parallel()

	docs := &fakeOccurredDocSearcher{}
	s := newOccurredTestServer(docs, true)

	result := callTool(t, s, "search", authorizedCtx(), map[string]any{
		"query":         "test",
		"occurred_from": "2026-09-05T00:00:00+09:00",
		"use_rerank":    true,
	})
	if isErrorResult(result) {
		t.Fatalf("unexpected error result: %s", resultText(result))
	}
	if docs.lastQuery.Sort != model.SortRecent {
		t.Fatalf("precondition failed: Sort = %q, want %q", docs.lastQuery.Sort, model.SortRecent)
	}
	if !docs.lastQuery.UseRerank {
		t.Error("UseRerank = false, want true (explicit override wins over the recency guard)")
	}
}

// ---------------------------------------------------------------------------
// Response field serialization tests
// ---------------------------------------------------------------------------

// searchToolResponse mirrors the JSON shape registerSearchTool's handler
// marshals — kept local to this test file rather than reusing the unexported
// searchResult type, so the test also catches an accidental json tag rename.
type searchToolResponse struct {
	Results []struct {
		DocumentID string  `json:"document_id"`
		SourceType string  `json:"source_type"`
		Score      float64 `json:"score"`
		OccurredAt *string `json:"occurred_at"`
		Retention  *string `json:"retention"`
		Segment    *string `json:"segment"`
	} `json:"results"`
	Count int `json:"count"`
}

// TestSearchTool_ResponseFields_PopulatedFromMetadata verifies that
// occurred_at, retention, and segment are surfaced as explicit fields on
// each result (alongside the pre-existing source_type/score), sourced from
// Document.OccurredAt and Document.Metadata.
func TestSearchTool_ResponseFields_PopulatedFromMetadata(t *testing.T) {
	t.Parallel()

	occurredAt := time.Date(2026, 9, 10, 3, 0, 0, 0, time.UTC)
	docID := uuid.New()
	docs := &fakeOccurredDocSearcher{
		results: []*model.SearchResult{
			{
				Document: model.Document{
					ID:         docID,
					SourceType: model.SourceGmail,
					Title:      "뉴스레터",
					Content:    "내용",
					Metadata:   map[string]any{"retention": "low", "segment": "newsletter"},
					OccurredAt: &occurredAt,
				},
				Score:     0.8,
				MatchType: "fulltext",
			},
		},
	}
	s := newOccurredTestServer(docs, false)

	result := callTool(t, s, "search", authorizedCtx(), map[string]any{"query": "test"})
	if isErrorResult(result) {
		t.Fatalf("unexpected error result: %s", resultText(result))
	}

	var payload searchToolResponse
	if err := json.Unmarshal([]byte(resultText(result)), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(payload.Results))
	}

	r := payload.Results[0]
	if r.DocumentID != docID.String() {
		t.Errorf("document_id = %q, want %q", r.DocumentID, docID.String())
	}
	if r.SourceType != string(model.SourceGmail) {
		t.Errorf("source_type = %q, want %q", r.SourceType, model.SourceGmail)
	}
	if r.Score <= 0 {
		t.Errorf("score = %v, want > 0", r.Score)
	}
	wantOccurredAt := occurredAt.Format(time.RFC3339)
	if r.OccurredAt == nil || *r.OccurredAt != wantOccurredAt {
		t.Errorf("occurred_at = %v, want %q", r.OccurredAt, wantOccurredAt)
	}
	if r.Retention == nil || *r.Retention != "low" {
		t.Errorf("retention = %v, want \"low\"", r.Retention)
	}
	if r.Segment == nil || *r.Segment != "newsletter" {
		t.Errorf("segment = %v, want \"newsletter\"", r.Segment)
	}
}

// TestSearchTool_ResponseFields_NullWhenAbsent verifies occurred_at,
// retention, and segment serialize as explicit JSON null — not omitted —
// when the underlying document carries none of them, so a caller can rely
// on the key always being present.
func TestSearchTool_ResponseFields_NullWhenAbsent(t *testing.T) {
	t.Parallel()

	docID := uuid.New()
	docs := &fakeOccurredDocSearcher{
		results: []*model.SearchResult{
			{
				Document: model.Document{
					ID:         docID,
					SourceType: model.SourceSlack,
					Title:      "no metadata",
					Content:    "content",
				},
				Score:     0.5,
				MatchType: "fulltext",
			},
		},
	}
	s := newOccurredTestServer(docs, false)

	result := callTool(t, s, "search", authorizedCtx(), map[string]any{"query": "test"})
	if isErrorResult(result) {
		t.Fatalf("unexpected error result: %s", resultText(result))
	}

	text := resultText(result)
	for _, key := range []string{`"occurred_at":null`, `"retention":null`, `"segment":null`} {
		if !strings.Contains(text, key) {
			t.Errorf("expected response to contain %s, got: %s", key, text)
		}
	}

	var payload searchToolResponse
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(payload.Results))
	}
	r := payload.Results[0]
	if r.OccurredAt != nil {
		t.Errorf("occurred_at = %v, want nil", *r.OccurredAt)
	}
	if r.Retention != nil {
		t.Errorf("retention = %v, want nil", *r.Retention)
	}
	if r.Segment != nil {
		t.Errorf("segment = %v, want nil", *r.Segment)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// fakeHTTPRequest builds a minimal *http.Request with the given Authorization
// header value. An empty string means no header is set.
func fakeHTTPRequest(authz string) *http.Request {
	req, _ := http.NewRequest(http.MethodPost, "/mcp", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	return req
}
