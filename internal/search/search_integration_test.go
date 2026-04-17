package search

import (
	"context"
	"errors"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// mockReranker is a test double for the Reranker interface.
type mockReranker struct {
	enabled bool
	fn      func(ctx context.Context, query string, docs []string) ([]RerankResult, error)
}

func (m *mockReranker) Enabled() bool { return m.enabled }

func (m *mockReranker) Rerank(ctx context.Context, query string, docs []string) ([]RerankResult, error) {
	return m.fn(ctx, query, docs)
}

// newServiceWithReranker constructs the minimal Service needed to call applyRerank.
func newServiceWithReranker(r Reranker) *Service {
	return &Service{reranker: r}
}

func makeResults(contents ...string) []*model.SearchResult {
	out := make([]*model.SearchResult, len(contents))
	for i, c := range contents {
		out[i] = &model.SearchResult{
			Document: model.Document{
				Title:   "title",
				Content: c,
			},
			Score: float64(len(contents) - i),
		}
	}
	return out
}

// TestApplyRerank_EmptyResults verifies that nil input returns an empty slice
// without error.
func TestApplyRerank_EmptyResults(t *testing.T) {
	t.Parallel()

	svc := newServiceWithReranker(&mockReranker{
		enabled: true,
		fn: func(_ context.Context, _ string, docs []string) ([]RerankResult, error) {
			return []RerankResult{}, nil
		},
	})

	got, err := svc.applyRerank(context.Background(), "query", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty slice, got %d elements", len(got))
	}
}

// TestApplyRerank_RerankerError verifies that a reranker error is propagated.
func TestApplyRerank_RerankerError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("reranker failure")
	svc := newServiceWithReranker(&mockReranker{
		enabled: true,
		fn: func(_ context.Context, _ string, _ []string) ([]RerankResult, error) {
			return nil, wantErr
		},
	})

	results := makeResults("doc a", "doc b")
	_, err := svc.applyRerank(context.Background(), "query", results)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("want %v, got %v", wantErr, err)
	}
}

// TestApplyRerank_OutOfBoundsIndex verifies that out-of-bounds indices returned
// by the reranker are silently skipped and do not cause a panic.
func TestApplyRerank_OutOfBoundsIndex(t *testing.T) {
	t.Parallel()

	svc := newServiceWithReranker(&mockReranker{
		enabled: true,
		fn: func(_ context.Context, _ string, docs []string) ([]RerankResult, error) {
			// Return one valid index and one out-of-bounds index.
			return []RerankResult{
				{Index: 0, Score: 0.9},
				{Index: len(docs) + 5, Score: 0.5}, // out of bounds
				{Index: -1, Score: 0.3},             // negative, also out of bounds
			}, nil
		},
	})

	results := makeResults("only doc")
	got, err := svc.applyRerank(context.Background(), "query", results)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only the valid index 0 should appear in the output.
	if len(got) != 1 {
		t.Fatalf("want 1 result (out-of-bounds skipped), got %d", len(got))
	}
	if got[0].Content != "only doc" {
		t.Errorf("unexpected content: %q", got[0].Content)
	}
}

// TestApplyRerank_NormalFlow verifies that results are reordered according to
// the reranker's returned indices and scores are updated.
func TestApplyRerank_NormalFlow(t *testing.T) {
	t.Parallel()

	// Reranker reverses order: doc at index 2 gets highest score.
	svc := newServiceWithReranker(&mockReranker{
		enabled: true,
		fn: func(_ context.Context, _ string, docs []string) ([]RerankResult, error) {
			return []RerankResult{
				{Index: 2, Score: 0.95},
				{Index: 0, Score: 0.72},
				{Index: 1, Score: 0.41},
			}, nil
		},
	})

	results := makeResults("doc-0", "doc-1", "doc-2")
	got, err := svc.applyRerank(context.Background(), "query", results)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 results, got %d", len(got))
	}

	// Verify reordering and score updates.
	wantContents := []string{"doc-2", "doc-0", "doc-1"}
	wantScores := []float64{0.95, 0.72, 0.41}
	for i, r := range got {
		if r.Content != wantContents[i] {
			t.Errorf("result[%d].Content = %q, want %q", i, r.Content, wantContents[i])
		}
		if r.Score != wantScores[i] {
			t.Errorf("result[%d].Score = %f, want %f", i, r.Score, wantScores[i])
		}
	}

	// Original slice must not be mutated (shallow copy check).
	if results[0].Score == 0.72 {
		t.Error("original results[0].Score was mutated")
	}
}
