// Package tune measures search-weight candidates against labelled feedback.
//
// It is deliberately the only place that knows how to turn a set of weights
// into a number. The holdout half of the labels is never visible here: this
// package receives a *dataset.HoldoutSet at most as something to call
// Evaluate on, and the Go compiler prevents it from reading anything inside.
package tune

import (
	"context"
	"fmt"

	"github.com/baekenough/second-brain/internal/dataset"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
)

// Searcher is the one capability the harness needs. *search.Service satisfies
// it.
type Searcher interface {
	Search(ctx context.Context, q model.SearchQuery) ([]*model.SearchResult, error)
}

// Compile-time proof that the production search service still fits.
var _ Searcher = (*search.Service)(nil)

// RunnerFor returns a dataset.RunFunc that searches with exactly the given
// weights.
//
// Weights travel on the query rather than through search.Service.WithWeights.
// WithWeights mutates the service and returns it, so evaluating a second
// candidate through the same instance would leave the first candidate's
// weights in place for any lane that did not overwrite them — a silent
// contamination that produces plausible-looking numbers for the wrong
// configuration. model.SearchQuery.Weights is per-request and takes precedence
// over the service-level value, which makes each evaluation independent by
// construction and lets a single service instance be reused safely.
//
// Defaults() is applied so that a candidate can never collapse to the zero
// value, which search.Service reads as "fall back to whatever this service was
// configured with".
//
// UseRerank and UseHyDE are forced off, not merely left unset:
//   - HyDE calls a remote LLM, so the same query would retrieve different
//     candidates on each sweep. A search whose result depends on the weather is
//     not something to tune against, and hundreds of sweeps would bill for it.
//   - The reranker is a separate HTTP service that the batch host is not
//     guaranteed to reach (macmini and ubuntu1 cannot address each other's
//     service ports). Baseline and candidate are compared under identical
//     conditions, so the relative comparison stays valid; the absolute numbers
//     are simply pre-rerank numbers, and the weights being tuned act on RRF
//     fusion, which happens before reranking anyway. Record rerank=false
//     alongside any promoted result so this is not mistaken for a live measure.
func RunnerFor(svc Searcher, w model.SearchWeights) dataset.RunFunc {
	weights := w.Defaults()
	return func(ctx context.Context, query string, k int) ([]string, error) {
		results, err := svc.Search(ctx, model.SearchQuery{
			Query:     query,
			Limit:     k,
			Weights:   weights,
			UseRerank: false,
			UseHyDE:   false,
		})
		if err != nil {
			// The query text is not included: it is the user's own words.
			return nil, fmt.Errorf("tune: search for %s: %w", dataset.QueryHash(query), err)
		}
		if k > 0 && len(results) > k {
			results = results[:k]
		}
		ids := make([]string, len(results))
		for i, r := range results {
			ids[i] = r.ID.String()
		}
		return ids, nil
	}
}

// Objective scores a measurement. It delegates to dataset.ObjectiveOf so that
// the optimiser and the promotion gate cannot end up with different ideas of
// what an improvement is.
func Objective(m dataset.Metrics) float64 {
	return dataset.ObjectiveOf(m.NDCG10, m.FPPenalty10)
}
