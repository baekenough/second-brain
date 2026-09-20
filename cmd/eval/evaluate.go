package main

import (
	"context"
	"fmt"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"golang.org/x/sync/errgroup"
	"time"
)

type evalSearcher interface {
	Search(context.Context, model.SearchQuery) ([]*model.SearchResult, error)
}
type evaluation struct {
	Metrics                                             search.EvalMetrics
	Attempted, Failed, PositiveQueries, NegativeQueries int
	FPPenalty10                                         float64
	Latencies                                           []float64
}

// Each input owns one result slot, including failed searches. Failures receive
// empty results and remain in the relevance denominator; they also fail the run.
// Negative-only questions contribute to FP, not undefined relevance metrics.
func evaluatePairs(ctx context.Context, svc evalSearcher, pairs []store.EvalPair, rerank bool) evaluation {
	results := make([][]string, len(pairs))
	positive := make([]map[string]bool, len(pairs))
	negative := make([]map[string]bool, len(pairs))
	latencies := make([]float64, len(pairs))
	failed := make([]bool, len(pairs))
	var group errgroup.Group
	group.SetLimit(10)
	for i, p := range pairs {
		positive[i] = map[string]bool{}
		negative[i] = map[string]bool{}
		for _, id := range p.RelevantDocIDs {
			positive[i][id] = true
		}
		for _, id := range p.IrrelevantDocIDs {
			negative[i][id] = true
		}
		group.Go(func() error {
			start := time.Now()
			rows, err := svc.Search(ctx, model.SearchQuery{Query: p.Query, Limit: 10, UseRerank: rerank})
			latencies[i] = float64(time.Since(start).Nanoseconds()) / 1e6
			if err != nil {
				failed[i] = true
				return nil
			}
			for _, row := range rows {
				results[i] = append(results[i], row.ID.String())
			}
			return nil
		})
	}
	_ = group.Wait()
	out := evaluation{Attempted: len(pairs), Latencies: latencies}
	var relevanceResults [][]string
	var relevanceLabels []map[string]bool
	for i := range pairs {
		if failed[i] {
			out.Failed++
		}
		if len(positive[i]) > 0 {
			out.PositiveQueries++
			relevanceResults = append(relevanceResults, results[i])
			relevanceLabels = append(relevanceLabels, positive[i])
		}
		if len(negative[i]) > 0 {
			out.NegativeQueries++
		}
	}
	out.Metrics = search.Aggregate(relevanceResults, relevanceLabels)
	out.FPPenalty10 = search.AggregateFPPenalty(results, negative, 10)
	return out
}

func (e evaluation) completionError() error {
	if e.Attempted == 0 {
		return fmt.Errorf("eval: no queries evaluated")
	}
	if e.Failed > 0 {
		return fmt.Errorf("eval: %d of %d searches failed; run is not a valid baseline", e.Failed, e.Attempted)
	}
	return nil
}
