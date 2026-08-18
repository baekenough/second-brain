package dataset

import (
	"context"
	"errors"
	"fmt"

	"github.com/baekenough/second-brain/internal/store"
)

// DefaultHoldoutBudget is how many times a HoldoutSet may be evaluated: one
// baseline measurement plus one candidate measurement. Repeatedly probing the
// holdout set and keeping whichever candidate scores best turns it into a
// second training set with extra steps, so the budget is a hard stop rather
// than a warning.
const DefaultHoldoutBudget = 2

// ErrHoldoutBudgetExhausted is returned by HoldoutSet.Evaluate once the
// evaluation budget is spent.
var ErrHoldoutBudgetExhausted = errors.New("dataset: holdout evaluation budget exhausted")

// Source supplies evaluation pairs for exactly one split at a time.
// *store.EvalStore satisfies it.
//
// The interface takes the split as an argument on purpose: there is no
// "give me everything" method to accidentally call, so no code path exists
// that could hand the whole labelled set to a tuner.
type Source interface {
	EvalPairsBySplit(ctx context.Context, split string) ([]store.EvalPair, error)
}

// Compile-time proof that the production store satisfies the split-only
// contract, so a signature change on either side breaks the build rather than
// the isolation guarantee.
var _ Source = (*store.EvalStore)(nil)

// TrainSet is the readable half. Coordinate search needs the raw pairs, so
// they are exposed.
type TrainSet struct {
	pairs []store.EvalPair
}

// Pairs returns a copy of the training pairs. The copy keeps a caller from
// mutating the set between evaluations, which would make a search run
// irreproducible.
func (t *TrainSet) Pairs() []store.EvalPair {
	out := make([]store.EvalPair, len(t.pairs))
	copy(out, t.pairs)
	return out
}

// Queries returns the number of training pairs (one pair per distinct query).
func (t *TrainSet) Queries() int { return len(t.pairs) }

// HoldoutSet is the closed half. Every field is unexported and the only
// exported methods are Evaluate and Queries, neither of which returns a query
// string, a document ID or an evaluation pair. A caller in another package can
// therefore measure against the holdout set but cannot look at it — the
// compiler enforces this, and TestHoldoutSet_ExportedSurfaceIsFrozen keeps the
// surface from quietly growing.
type HoldoutSet struct {
	pairs  []store.EvalPair
	budget int
}

// Queries returns how many holdout queries exist. It is a count, not the
// queries — the promotion gate needs the size of the sample and nothing more.
func (h *HoldoutSet) Queries() int { return len(h.pairs) }

// Load reads the two splits with two separate queries and returns them as
// distinct types.
//
// Splits are read separately rather than read whole and partitioned in memory:
// a single "read everything" call would put the holdout rows in a variable that
// some future caller could pass along, and the point of this package is that
// such a variable never exists outside it.
func Load(ctx context.Context, src Source) (*TrainSet, *HoldoutSet, error) {
	trainPairs, err := src.EvalPairsBySplit(ctx, SplitTrain)
	if err != nil {
		return nil, nil, fmt.Errorf("dataset: load train split: %w", err)
	}
	holdoutPairs, err := src.EvalPairsBySplit(ctx, SplitHoldout)
	if err != nil {
		return nil, nil, fmt.Errorf("dataset: load holdout split: %w", err)
	}

	// Contamination check. The split is a pure function of the normalised
	// query, so an overlap means the Go rule and the SQL rule have drifted
	// apart. That is not a degraded state to log and continue from: every
	// holdout number computed afterwards would be measured on data the
	// optimiser trained on. Fail closed.
	trainQueries := make(map[string]struct{}, len(trainPairs))
	for _, p := range trainPairs {
		trainQueries[Normalize(p.Query)] = struct{}{}
	}
	for _, p := range holdoutPairs {
		if _, dup := trainQueries[Normalize(p.Query)]; dup {
			return nil, nil, fmt.Errorf(
				"dataset: query hash %s appears in both splits; split rule drift, refusing to load",
				queryHash(p.Query))
		}
	}

	return &TrainSet{pairs: trainPairs},
		&HoldoutSet{pairs: holdoutPairs, budget: DefaultHoldoutBudget},
		nil
}
