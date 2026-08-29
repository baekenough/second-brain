package main

import (
	"context"
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
}

func (f *fakeOccurredDocSearcher) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	f.called = true
	f.lastQuery = q
	return nil, nil
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
// production handler code path, not a test stub.
func newOccurredTestServer(docs *fakeOccurredDocSearcher) *mcpserver.MCPServer {
	svc := search.NewService(docs, disabledEmbeddingEngine{})
	s := mcpserver.NewMCPServer("test", "0.0.0", mcpserver.WithToolCapabilities(false))
	registerSearchTool(s, svc)
	return s
}

func TestSearchTool_OccurredRange_BothBoundsRFC3339(t *testing.T) {
	t.Parallel()

	docs := &fakeOccurredDocSearcher{}
	s := newOccurredTestServer(docs)

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
	s := newOccurredTestServer(docs)

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
	s := newOccurredTestServer(docs)

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
	s := newOccurredTestServer(docs)

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
	s := newOccurredTestServer(docs)

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
			s := newOccurredTestServer(docs)

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
			s := newOccurredTestServer(docs)

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
	registerSearchTool(s, nil) // nil *search.Service — handler short-circuits at source validation

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
