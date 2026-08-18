package dataset

import (
	"context"
	"fmt"

	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"golang.org/x/sync/errgroup"
)

// evalK is the cutoff every metric in this package uses. It is fixed at 10 to
// match NDCG@10 / FP@10, the two numbers the promotion gate is written against.
const evalK = 10

// maxEvalConcurrency bounds how many queries are searched at once. It is well
// under the Postgres pool size so that an evaluation run cannot starve the
// service sharing the database.
const maxEvalConcurrency = 4

// RunFunc executes one search and returns the top-k document IDs in rank order.
// It is the only thing a caller hands to HoldoutSet.Evaluate — the caller
// supplies the searcher, the set supplies the questions, and only aggregate
// numbers come back.
type RunFunc func(ctx context.Context, query string, k int) ([]string, error)

// Metrics is the entire output of an evaluation: scalars, no per-query detail.
type Metrics struct {
	NDCG10      float64 `json:"ndcg10"`
	MRR10       float64 `json:"mrr10"`
	FPPenalty10 float64 `json:"fp_penalty10"`
	Objective   float64 `json:"objective"`
	Pairs       int     `json:"pairs"`
	Queries     int     `json:"queries"`
}

// ObjectiveOf is the single objective function used for both search and
// promotion: NDCG@10 penalised by half the false-positive rate at 10 (plan
// §2.4). Defined once here so a tuner and a gate cannot disagree about what
// "better" means.
func ObjectiveOf(ndcg10, fpPenalty10 float64) float64 {
	return ndcg10 - 0.5*fpPenalty10
}

// Evaluate runs the supplied searcher over every holdout query and returns the
// aggregate metrics. The pairs themselves never leave this method.
//
// The budget is spent on entry, including for a run that later fails: an
// attempt that reached the search path has already observed the holdout set,
// and letting failures be free would make an unlimited probing loop trivial to
// write by accident.
func (h *HoldoutSet) Evaluate(ctx context.Context, run RunFunc) (Metrics, error) {
	if h.budget <= 0 {
		return Metrics{}, ErrHoldoutBudgetExhausted
	}
	h.budget--
	return evaluatePairs(ctx, h.pairs, run)
}

// EvaluateTrain runs the searcher over the training pairs. Unlike the holdout
// path this is unbudgeted — the train split is what coordinate search is
// supposed to be hammering.
func (t *TrainSet) EvaluateTrain(ctx context.Context, run RunFunc) (Metrics, error) {
	return evaluatePairs(ctx, t.pairs, run)
}

// evaluatePairs searches every pair with bounded concurrency and aggregates.
//
// Results are written into a pre-sized slice at the pair's index rather than
// appended, so the aggregate does not depend on goroutine completion order. A
// single failed search aborts the whole evaluation: scoring a failed query as
// zero would blur "these weights are worse" into "the database hiccuped", and
// the entire point of this package is to be able to tell those apart.
func evaluatePairs(ctx context.Context, pairs []store.EvalPair, run RunFunc) (Metrics, error) {
	if len(pairs) == 0 {
		return Metrics{}, nil
	}

	results := make([][]string, len(pairs))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxEvalConcurrency)
	for i, p := range pairs {
		i, p := i, p
		g.Go(func() error {
			docIDs, err := run(gctx, p.Query, evalK)
			if err != nil {
				// query_hash, never the query: see QueryHash.
				return fmt.Errorf("dataset: evaluate query %s: %w", queryHash(p.Query), err)
			}
			results[i] = docIDs
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return Metrics{}, err
	}

	relevant := make([]map[string]bool, len(pairs))
	irrelevant := make([]map[string]bool, len(pairs))
	for i, p := range pairs {
		relevant[i] = toSet(p.RelevantDocIDs)
		irrelevant[i] = toSet(p.IrrelevantDocIDs)
	}

	agg := search.Aggregate(results, relevant)
	fp := search.AggregateFPPenalty(results, irrelevant, evalK)

	return Metrics{
		NDCG10:      agg.NDCG10,
		MRR10:       agg.MRR10,
		FPPenalty10: fp,
		Objective:   ObjectiveOf(agg.NDCG10, fp),
		Pairs:       len(pairs),
		Queries:     distinctQueries(pairs),
	}, nil
}

func toSet(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// distinctQueries counts normalised queries. EvalPair is already one row per
// query today, but the promotion gate's "30 distinct queries" threshold must
// not silently inflate if that ever stops being true.
func distinctQueries(pairs []store.EvalPair) int {
	seen := make(map[string]struct{}, len(pairs))
	for _, p := range pairs {
		seen[Normalize(p.Query)] = struct{}{}
	}
	return len(seen)
}
