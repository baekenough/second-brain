package curation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"

	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
)

// CuratedResult wraps a search result with an LLM-generated summary.
// Original document data is always preserved untouched.
type CuratedResult struct {
	Summary         string         `json:"summary"`
	Original        model.Document `json:"original"`
	Relevance       float64        `json:"relevance"`
	RelevanceReason string         `json:"relevance_reason,omitempty"`
}

type llmRankEntry struct {
	Index           int     `json:"index"`
	Summary         string  `json:"summary"`
	Relevance       float64 `json:"relevance"`
	RelevanceReason string  `json:"relevance_reason,omitempty"`
}

// Curator re-ranks and summarizes search results via LLM.
type Curator struct {
	llm llm.Completer
}

// New returns a Curator. If llm is nil, Curate returns passthrough results.
func New(c llm.Completer) *Curator {
	return &Curator{llm: c}
}

// Curate re-ranks and lightly summarizes the given search results.
// Original data is ALWAYS preserved — never modified.
// When the LLM is nil or disabled, results are returned as-is.
func (c *Curator) Curate(ctx context.Context, query string, results []*model.SearchResult) ([]CuratedResult, error) {
	if c.llm == nil || !c.llm.Enabled() {
		return passthrough(results), nil
	}

	systemPrompt := `You are a search result curator. Given a query and search results:
1. Re-rank results by relevance (most relevant first)
2. Generate a brief summary (1-2 sentences) for each result
3. Assign a relevance score (0.0 to 1.0)
4. Filter out clearly irrelevant results (relevance < 0.3)

IMPORTANT: Do NOT modify original content. Summaries should be lightweight.
For Korean content, write summaries in Korean.

Respond with a JSON array only, no markdown fencing:
[{"index": 0, "summary": "...", "relevance": 0.95, "relevance_reason": "..."}]

"index" refers to the position in the input results array (0-indexed).`

	type inputItem struct {
		Index   int    `json:"index"`
		Title   string `json:"title"`
		Content string `json:"content"`
		Source  string `json:"source"`
	}
	items := make([]inputItem, len(results))
	for i, r := range results {
		content := r.Content
		if len(content) > 2000 {
			content = content[:2000] + "..."
		}
		items[i] = inputItem{
			Index:   i,
			Title:   r.Title,
			Content: content,
			Source:  string(r.SourceType),
		}
	}

	userPrompt := fmt.Sprintf("Query: %s\n\nResults:\n%s", query, mustMarshal(items))

	response, err := c.llm.CompleteWithMessages(ctx, systemPrompt, []llm.Message{
		{Role: "user", Content: userPrompt},
	})
	if err != nil {
		slog.Warn("curation: LLM call failed, returning passthrough", "error", err)
		return passthrough(results), nil
	}

	var rankings []llmRankEntry
	if err := json.Unmarshal([]byte(response), &rankings); err != nil {
		slog.Warn("curation: failed to parse LLM response, returning passthrough",
			"error", err, "response", response)
		return passthrough(results), nil
	}

	sort.Slice(rankings, func(i, j int) bool {
		return rankings[i].Relevance > rankings[j].Relevance
	})

	curated := make([]CuratedResult, 0, len(rankings))
	for _, rank := range rankings {
		if rank.Index < 0 || rank.Index >= len(results) {
			continue
		}
		if rank.Relevance < 0.3 {
			continue
		}
		curated = append(curated, CuratedResult{
			Summary:         rank.Summary,
			Original:        results[rank.Index].Document,
			Relevance:       rank.Relevance,
			RelevanceReason: rank.RelevanceReason,
		})
	}

	return curated, nil
}

func passthrough(results []*model.SearchResult) []CuratedResult {
	out := make([]CuratedResult, len(results))
	for i, r := range results {
		out[i] = CuratedResult{
			Original:  r.Document,
			Relevance: r.Score,
		}
	}
	return out
}

func mustMarshal(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
