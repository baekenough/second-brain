package search

import (
	"context"
	"errors"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
	"testing"
)

type queryCapturingEmbedder struct {
	stubEnabledEmbedder
	text string
}

func (e *queryCapturingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	e.text = text
	return []float32{.1, .2, .3}, nil
}

type queryCapturingStore struct {
	mockDocSearcher
	query string
}

func (s *queryCapturingStore) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	s.query = q.Query
	return s.results, nil
}

type hydeIsolationCompleter struct{ err error }

func (c hydeIsolationCompleter) Enabled() bool { return true }
func (c hydeIsolationCompleter) CompleteWithMessages(context.Context, string, []llm.Message) (string, error) {
	return "hypothetical invented vocabulary", c.err
}

func TestHyDEExpansionIsRestrictedToEmbedding(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "expanded", true: "fallback"}[failure], func(t *testing.T) {
			embed := &queryCapturingEmbedder{}
			store := &queryCapturingStore{mockDocSearcher: mockDocSearcher{results: []*model.SearchResult{makeSearchResult(uuid.New(), "A", 2), makeSearchResult(uuid.New(), "B", 1)}}}
			rerankQuery := ""
			rerank := &mockReranker{enabled: true, fn: func(_ context.Context, q string, _ []string) ([]RerankResult, error) {
				rerankQuery = q
				return []RerankResult{{Index: 0, Score: 1}, {Index: 1, Score: .5}}, nil
			}}
			completer := hydeIsolationCompleter{}
			if failure {
				completer.err = errors.New("synthetic timeout")
			}
			_, err := NewService(store, embed).WithLLM(completer).WithReranker(rerank).Search(context.Background(), model.SearchQuery{Query: "original question", UseHyDE: true, UseRerank: true, Limit: 2})
			if err != nil {
				t.Fatal(err)
			}
			if store.query != "original question" || rerankQuery != "original question" {
				t.Fatal("hypothetical vocabulary contaminated lexical query or reranking")
			}
			expected := "original question"
			if !failure {
				expected += "\n\nhypothetical invented vocabulary"
			}
			if embed.text != expected {
				t.Fatalf("embedding expansion mismatch: failure=%t", failure)
			}
		})
	}
}
