package search

import "math"

// NDCGK computes Normalised Discounted Cumulative Gain at rank K for a single
// query. results contains document IDs in ranked order (position 0 = rank 1).
// relevant is the ground-truth set of relevant document IDs. k is the cutoff;
// if k <= 0 or k > len(results), the full results slice is used.
//
// Returns 0 when relevant is empty or no relevant result appears in the top-K.
func NDCGK(results []string, relevant map[string]bool, k int) float64 {
	if len(relevant) == 0 {
		return 0
	}

	limit := len(results)
	if k > 0 && k < limit {
		limit = k
	}

	dcg := 0.0
	for i := 0; i < limit; i++ {
		if relevant[results[i]] {
			// rank is 1-indexed: i+1; discount = log2(rank + 1) = log2(i + 2)
			dcg += 1.0 / math.Log2(float64(i+2))
		}
	}

	// Ideal DCG: top-K filled with as many relevant docs as possible.
	idealLen := len(relevant)
	if limit < idealLen {
		idealLen = limit
	}
	idcg := 0.0
	for i := 0; i < idealLen; i++ {
		idcg += 1.0 / math.Log2(float64(i+2))
	}

	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

// MRRK computes the Mean Reciprocal Rank for a single query within the top-K
// results. Returns 1/rank of the first relevant result, or 0 if no relevant
// result appears within the top-K. k <= 0 means no cutoff.
func MRRK(results []string, relevant map[string]bool, k int) float64 {
	limit := len(results)
	if k > 0 && k < limit {
		limit = k
	}

	for i := 0; i < limit; i++ {
		if relevant[results[i]] {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

// EvalMetrics aggregates evaluation metrics over an eval set.
// Each field is the macro-average over all query/relevance pairs.
type EvalMetrics struct {
	NDCG5  float64 // NDCG@5 macro-average
	NDCG10 float64 // NDCG@10 macro-average
	MRR10  float64 // MRR@10 macro-average
	Pairs  int     // number of query/relevance pairs evaluated
}

// Aggregate computes EvalMetrics from parallel slices of results and relevance
// judgements. results[i] is the ranked result list for query i; relevant[i] is
// the ground-truth set for query i. The slices must have equal length.
//
// Returns a zero EvalMetrics when the input is empty.
func Aggregate(results [][]string, relevant []map[string]bool) EvalMetrics {
	n := len(results)
	if n == 0 {
		return EvalMetrics{}
	}

	var ndcg5, ndcg10, mrr10 float64
	for i := 0; i < n; i++ {
		ndcg5 += NDCGK(results[i], relevant[i], 5)
		ndcg10 += NDCGK(results[i], relevant[i], 10)
		mrr10 += MRRK(results[i], relevant[i], 10)
	}

	fn := float64(n)
	return EvalMetrics{
		NDCG5:  ndcg5 / fn,
		NDCG10: ndcg10 / fn,
		MRR10:  mrr10 / fn,
		Pairs:  n,
	}
}
