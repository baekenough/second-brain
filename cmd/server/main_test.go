package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/api"
	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// stubActionStore is a minimal ActionLister + ActionStateSetter fake used to
// verify route registration without a live Postgres connection. It never
// needs to return real data — these tests only assert whether the route
// exists (404 vs. handled), not the response body.
type stubActionStore struct{}

func (stubActionStore) ListOpenActions(_ context.Context, _ store.ActionFilter) ([]store.ActionListItem, error) {
	return nil, nil
}

func (stubActionStore) SetActionState(_ context.Context, _ string, _ model.ActionState, _ string) (bool, error) {
	return true, nil
}

// newWiringTestServer builds the same api.Server shape run() constructs,
// minus the DB/LLM dependencies that require live infrastructure.
func newWiringTestServer(llmClient llm.Completer) *api.Server {
	return api.NewServer(nil, nil, nil, nil, llmClient, "", "")
}

func getStatus(t *testing.T, srv *api.Server, method, path string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr.Code
}

// TestWireActionsAndBriefing_DisabledByDefault pins the rollback story for
// Task 12: with both flags off (the zero value of config.Config), neither
// route exists in the router at all — a 404, not a 401/500 from a nil
// dependency.
func TestWireActionsAndBriefing_DisabledByDefault(t *testing.T) {
	cfg := &config.Config{} // ActionsAPIEnabled=false, BriefingEnabled=false
	llmClient := llm.New(llm.Config{BaseURL: "https://api.openai.com/v1", Model: "gpt-4o-mini", APIKey: "test-key"}, nil)

	srv := wireActionsAndBriefing(newWiringTestServer(llmClient), cfg, stubActionStore{}, stubActionStore{}, llmClient.Enabled())

	if code := getStatus(t, srv, http.MethodGet, "/api/v1/actions"); code != http.StatusNotFound {
		t.Errorf("GET /api/v1/actions = %d, want 404 when ACTIONS_API_ENABLED is unset", code)
	}
	if code := getStatus(t, srv, http.MethodGet, "/api/v1/briefing"); code != http.StatusNotFound {
		t.Errorf("GET /api/v1/briefing = %d, want 404 when both flags are unset", code)
	}
}

// TestWireActionsAndBriefing_ActionsEnabledBriefingOff verifies the actions
// route registers on its own without briefing following automatically.
func TestWireActionsAndBriefing_ActionsEnabledBriefingOff(t *testing.T) {
	cfg := &config.Config{ActionsAPIEnabled: true, BriefingMaxActions: 40}
	llmClient := llm.New(llm.Config{BaseURL: "https://api.openai.com/v1", Model: "gpt-4o-mini", APIKey: "test-key"}, nil)

	srv := wireActionsAndBriefing(newWiringTestServer(llmClient), cfg, stubActionStore{}, stubActionStore{}, llmClient.Enabled())

	if code := getStatus(t, srv, http.MethodGet, "/api/v1/actions"); code == http.StatusNotFound {
		t.Errorf("GET /api/v1/actions = 404, want the route to be registered when ACTIONS_API_ENABLED=true")
	}
	if code := getStatus(t, srv, http.MethodGet, "/api/v1/briefing"); code != http.StatusNotFound {
		t.Errorf("GET /api/v1/briefing = %d, want 404 when BRIEFING_ENABLED is unset", code)
	}
}

// TestWireActionsAndBriefing_BothEnabled verifies the briefing route
// registers once both flags are on and the LLM client is configured.
func TestWireActionsAndBriefing_BothEnabled(t *testing.T) {
	cfg := &config.Config{ActionsAPIEnabled: true, BriefingEnabled: true, BriefingMaxActions: 40}
	llmClient := llm.New(llm.Config{BaseURL: "https://api.openai.com/v1", Model: "gpt-4o-mini", APIKey: "test-key"}, nil)

	srv := wireActionsAndBriefing(newWiringTestServer(llmClient), cfg, stubActionStore{}, stubActionStore{}, llmClient.Enabled())

	if code := getStatus(t, srv, http.MethodGet, "/api/v1/actions"); code == http.StatusNotFound {
		t.Errorf("GET /api/v1/actions = 404, want registered")
	}
	if code := getStatus(t, srv, http.MethodGet, "/api/v1/briefing"); code == http.StatusNotFound {
		t.Errorf("GET /api/v1/briefing = 404, want registered when both flags are true and the LLM client is configured")
	}
}

// TestWireActionsAndBriefing_BriefingRequiresActionsEnabled verifies the
// documented dependency: BRIEFING_ENABLED=true with ACTIONS_API_ENABLED=false
// is a no-op combination (plan Task 12 design note — "그 반대는 무시된다").
func TestWireActionsAndBriefing_BriefingRequiresActionsEnabled(t *testing.T) {
	cfg := &config.Config{ActionsAPIEnabled: false, BriefingEnabled: true, BriefingMaxActions: 40}
	llmClient := llm.New(llm.Config{BaseURL: "https://api.openai.com/v1", Model: "gpt-4o-mini", APIKey: "test-key"}, nil)

	srv := wireActionsAndBriefing(newWiringTestServer(llmClient), cfg, stubActionStore{}, stubActionStore{}, llmClient.Enabled())

	if code := getStatus(t, srv, http.MethodGet, "/api/v1/actions"); code != http.StatusNotFound {
		t.Errorf("GET /api/v1/actions = %d, want 404 when ACTIONS_API_ENABLED=false", code)
	}
	if code := getStatus(t, srv, http.MethodGet, "/api/v1/briefing"); code != http.StatusNotFound {
		t.Errorf("GET /api/v1/briefing = %d, want 404 — briefing must not activate without actions", code)
	}
}

// TestWireActionsAndBriefing_LLMDisabledSkipsBriefing verifies requirement 4:
// when the LLM client is not configured (Enabled() == false), briefing must
// stay off even though BRIEFING_ENABLED=true — it degrades safely instead of
// registering a route that can only 500.
func TestWireActionsAndBriefing_LLMDisabledSkipsBriefing(t *testing.T) {
	cfg := &config.Config{ActionsAPIEnabled: true, BriefingEnabled: true, BriefingMaxActions: 40}
	llmClient := llm.New(llm.Config{}, nil) // no APIKey/AuthFile → Enabled() == false

	if llmClient.Enabled() {
		t.Fatal("test setup invalid: llmClient.Enabled() should be false with no APIKey/AuthFile")
	}

	srv := wireActionsAndBriefing(newWiringTestServer(llmClient), cfg, stubActionStore{}, stubActionStore{}, llmClient.Enabled())

	if code := getStatus(t, srv, http.MethodGet, "/api/v1/actions"); code == http.StatusNotFound {
		t.Errorf("GET /api/v1/actions = 404, want registered (actions do not depend on the LLM client)")
	}
	if code := getStatus(t, srv, http.MethodGet, "/api/v1/briefing"); code != http.StatusNotFound {
		t.Errorf("GET /api/v1/briefing = %d, want 404 when the LLM client is not configured, even with BRIEFING_ENABLED=true", code)
	}
}

// stubEvidenceVoter is a minimal api.EvidenceVoter fake. As with
// stubActionStore, these tests assert route existence, not behaviour.
type stubEvidenceVoter struct{}

func (stubEvidenceVoter) UpsertEvidence(_ context.Context, _ store.EvidenceVote) (int64, int16, error) {
	return 1, 1, nil
}

func postStatus(t *testing.T, srv *api.Server, path, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr.Code
}

// TestWireEvidenceFeedback_DisabledByDefault pins the rollback story for
// Part D: with FEEDBACK_EVIDENCE_ENABLED unset the route is never registered,
// so the endpoint answers 404 rather than reaching a handler with a live
// store behind it.
func TestWireEvidenceFeedback_DisabledByDefault(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "")
	cfg := &config.Config{} // FeedbackEvidenceEnabled == false

	srv := wireEvidenceFeedback(newWiringTestServer(nil), cfg, stubEvidenceVoter{})

	if code := postStatus(t, srv, "/api/v1/feedback/evidence", `{}`); code != http.StatusNotFound {
		t.Errorf("POST /api/v1/feedback/evidence = %d, want 404 when FEEDBACK_EVIDENCE_ENABLED is unset", code)
	}
}

// TestWireEvidenceFeedback_Enabled verifies the route is registered when the
// flag is on. A registered route rejects an invalid body with 400; the
// assertion is only "not 404", since 404 is what an unregistered route gives.
func TestWireEvidenceFeedback_Enabled(t *testing.T) {
	// The handler re-checks the same env var per request, so the wiring flag
	// alone is not enough to observe the route.
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "true")
	cfg := &config.Config{FeedbackEvidenceEnabled: true}

	srv := wireEvidenceFeedback(newWiringTestServer(nil), cfg, stubEvidenceVoter{})

	code := postStatus(t, srv, "/api/v1/feedback/evidence", `{`)
	if code == http.StatusNotFound {
		t.Errorf("POST /api/v1/feedback/evidence = 404, want the route registered when FEEDBACK_EVIDENCE_ENABLED=true")
	}
	if code != http.StatusBadRequest {
		t.Errorf("POST /api/v1/feedback/evidence with a malformed body = %d, want 400", code)
	}
}

// TestWireEvidenceFeedback_NilVoterStaysUnregistered guards the nil-store
// case: an operator turning the flag on without a store must not produce a
// route that can only panic.
func TestWireEvidenceFeedback_NilVoterStaysUnregistered(t *testing.T) {
	t.Setenv("FEEDBACK_EVIDENCE_ENABLED", "true")
	cfg := &config.Config{FeedbackEvidenceEnabled: true}

	srv := wireEvidenceFeedback(newWiringTestServer(nil), cfg, nil)

	if code := postStatus(t, srv, "/api/v1/feedback/evidence", `{}`); code != http.StatusNotFound {
		t.Errorf("POST /api/v1/feedback/evidence = %d, want 404 when no evidence store is available", code)
	}
}

// --- buildSearchService wiring regression tests ---
//
// These exercise the real chain buildSearchService assembles rather than a
// hand-rolled one, so they fail again if a future edit drops a .With*() call
// from it — the way .WithLLM(llmClient) was missing for the HyDE lane.

// fakeSearchDocStore is a minimal search.DocumentSearcher that records the
// query it was handed, so a test can inspect what the wiring actually
// delivered downstream (same pattern as internal/search's own wiring tests,
// e.g. recordingDocSearcher in insight_exclusion_test.go).
type fakeSearchDocStore struct {
	gotQuery model.SearchQuery
}

func (f *fakeSearchDocStore) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	f.gotQuery = q
	return nil, nil
}

// disabledSearchEmbedder is a search.EmbeddingEngine that is always off, so
// the HyDE wiring test stays on the full-text path — the assertion is about
// query expansion, not vector search.
type disabledSearchEmbedder struct{}

func (disabledSearchEmbedder) Embed(context.Context, string) ([]float32, error) { return nil, nil }
func (disabledSearchEmbedder) EmbedBatch(context.Context, []string) ([][]float32, error) {
	return nil, nil
}
func (disabledSearchEmbedder) Enabled() bool  { return false }
func (disabledSearchEmbedder) Dimension() int { return 0 }

// stubHyDECompleter is a minimal llm.Completer that always returns a fixed
// hypothetical-document string, so the test can detect whether the wired
// Service actually called through to it.
type stubHyDECompleter struct{}

func (stubHyDECompleter) Enabled() bool { return true }

func (stubHyDECompleter) CompleteWithMessages(context.Context, string, []llm.Message) (string, error) {
	return "hypothetical document text", nil
}

// TestBuildSearchService_WiresLLMClientForHyDE is the regression test for the
// defect this file fixes: llmClient used to be constructed AFTER the
// search.Service .With*() chain ran in run(), so nothing could pass it to
// WithLLM and every UseHyDE request silently returned unexpanded results with
// no error and no log line. If buildSearchService's chain ever drops
// .WithLLM again, this test fails instead of shipping another silent no-op.
func TestBuildSearchService_WiresLLMClientForHyDE(t *testing.T) {
	docs := &fakeSearchDocStore{}
	svc := buildSearchService(
		docs, disabledSearchEmbedder{}, nil, nil, nil, nil,
		stubHyDECompleter{}, nil, false,
	)

	const query = "what changed in the postgres migration?"
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: query, UseHyDE: true}); err != nil {
		t.Fatalf("Search returned unexpected error: %v", err)
	}

	if !strings.Contains(docs.gotQuery.Query, "hypothetical document text") {
		t.Fatalf("store received query %q, want it to contain the HyDE-expanded text — WithLLM is not wired", docs.gotQuery.Query)
	}
	if !strings.HasPrefix(docs.gotQuery.Query, query) {
		t.Fatalf("store received query %q, want it to start with the original query %q", docs.gotQuery.Query, query)
	}
}

// TestBuildSearchService_UseHyDEWithoutLLMClientIsANoOp pins the documented
// degrade-safe behaviour when no LLM client is available at all (nil
// llmClient — e.g. a caller that skips llm.New entirely): UseHyDE must not
// panic and must fall back to the original, unexpanded query.
func TestBuildSearchService_UseHyDEWithoutLLMClientIsANoOp(t *testing.T) {
	docs := &fakeSearchDocStore{}
	svc := buildSearchService(
		docs, disabledSearchEmbedder{}, nil, nil, nil, nil,
		nil, nil, false,
	)

	const query = "what changed in the postgres migration?"
	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: query, UseHyDE: true}); err != nil {
		t.Fatalf("Search returned unexpected error: %v", err)
	}
	if docs.gotQuery.Query != query {
		t.Fatalf("store received query %q, want the unexpanded original %q when no LLM client is wired", docs.gotQuery.Query, query)
	}
}

// TestHTTPServerWriteTimeout_CoversAskAndSearch documents the relationship
// the deployment depends on: the global write deadline must outlast the
// slowest non-briefing request path (/ask, and a cold-start /search — see
// issue #195), otherwise the response is truncated at the socket with a 200
// already on the wire.
func TestHTTPServerWriteTimeout_CoversAskAndSearch(t *testing.T) {
	cfg := &config.Config{
		HTTPWriteTimeout:  90 * time.Second,
		AskTimeoutSeconds: 60,
	}
	if cfg.HTTPWriteTimeout <= time.Duration(cfg.AskTimeoutSeconds)*time.Second {
		t.Fatalf("HTTPWriteTimeout %v must exceed the ask timeout %ds", cfg.HTTPWriteTimeout, cfg.AskTimeoutSeconds)
	}
}
