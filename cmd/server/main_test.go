package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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
