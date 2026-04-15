package curation

import (
	"context"
	"testing"

	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
)

type mockCompleter struct {
	response string
	err      error
}

func (m *mockCompleter) CompleteWithMessages(_ context.Context, _ string, _ []llm.Message) (string, error) {
	return m.response, m.err
}

func (m *mockCompleter) Enabled() bool { return true }

func TestCurator_Curate_ReturnsOriginal(t *testing.T) {
	llmResponse := `[{"index":0,"summary":"Onboarding guide overview","relevance":0.95}]`
	curator := New(&mockCompleter{response: llmResponse})

	results := []*model.SearchResult{
		{
			Document: model.Document{
				Title:   "Onboarding Guide",
				Content: "Welcome to the team...",
			},
			Score: 0.8,
		},
	}

	curated, err := curator.Curate(context.Background(), "onboarding", results)
	if err != nil {
		t.Fatalf("Curate() error = %v", err)
	}
	if len(curated) != 1 {
		t.Fatalf("Curate() returned %d results, want 1", len(curated))
	}
	if curated[0].Summary == "" {
		t.Error("Curate() result has empty summary")
	}
	if curated[0].Original.Title != "Onboarding Guide" {
		t.Errorf("original.title = %q, want %q", curated[0].Original.Title, "Onboarding Guide")
	}
	if curated[0].Original.Content != "Welcome to the team..." {
		t.Error("original content was modified — must be preserved exactly")
	}
	if curated[0].Relevance != 0.95 {
		t.Errorf("relevance = %v, want 0.95", curated[0].Relevance)
	}
}

func TestCurator_Curate_NilLLM_ReturnsPassthrough(t *testing.T) {
	curator := New(nil)

	results := []*model.SearchResult{
		{
			Document: model.Document{Title: "Doc1", Content: "content1"},
			Score:    0.9,
		},
	}

	curated, err := curator.Curate(context.Background(), "query", results)
	if err != nil {
		t.Fatalf("Curate() error = %v", err)
	}
	if len(curated) != 1 {
		t.Fatalf("returned %d results, want 1", len(curated))
	}
	if curated[0].Original.Title != "Doc1" {
		t.Errorf("passthrough title = %q, want Doc1", curated[0].Original.Title)
	}
	if curated[0].Relevance != 0.9 {
		t.Errorf("passthrough relevance = %v, want 0.9 (original score)", curated[0].Relevance)
	}
}

func TestCurator_Curate_FiltersLowRelevance(t *testing.T) {
	llmResponse := `[{"index":0,"summary":"relevant","relevance":0.9},{"index":1,"summary":"noise","relevance":0.1}]`
	curator := New(&mockCompleter{response: llmResponse})

	results := []*model.SearchResult{
		{Document: model.Document{Title: "Good"}, Score: 0.8},
		{Document: model.Document{Title: "Bad"}, Score: 0.2},
	}

	curated, err := curator.Curate(context.Background(), "test", results)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(curated) != 1 {
		t.Fatalf("returned %d results, want 1 (noise filtered)", len(curated))
	}
	if curated[0].Original.Title != "Good" {
		t.Errorf("title = %q, want Good", curated[0].Original.Title)
	}
}

func TestCurator_Curate_LLMError_ReturnsPassthrough(t *testing.T) {
	curator := New(&mockCompleter{err: context.DeadlineExceeded})

	results := []*model.SearchResult{
		{Document: model.Document{Title: "Doc"}, Score: 0.5},
	}

	curated, err := curator.Curate(context.Background(), "test", results)
	if err != nil {
		t.Fatalf("should not return error on LLM failure, got %v", err)
	}
	if len(curated) != 1 || curated[0].Original.Title != "Doc" {
		t.Error("expected passthrough on LLM error")
	}
}
