package evalcompare

import (
	"hash/fnv"
	"math/rand/v2"
	"sort"
)

// buildGroup computes the full comparison for one slice: n/mean/CI/verdict
// for ndcg10, recall10 and fp10, the median-only latency summary, and the
// worst (most regressed) query IDs by ndcg10 delta. bonferroniM is the
// Bonferroni divisor for GroupMetric.BonferroniSignificant — 0 for the
// overall (gating) group, which skips that computation, and the number of
// non-overall groups in the report otherwise (see Compare).
func buildGroup(name string, ids []string, byID map[string]queryDelta, opts Options, provenanceUnknown bool, bonferroniM int) Group {
	group := Group{Name: name}

	_, ndcgValues := collect(ids, byID, func(d queryDelta) *float64 { return d.NDCG10 })
	group.NDCG10 = groupMetric(name, "ndcg10", ndcgValues, opts, provenanceUnknown, bonferroniM)

	_, recallValues := collect(ids, byID, func(d queryDelta) *float64 { return d.Recall10 })
	group.Recall10 = groupMetric(name, "recall10", recallValues, opts, provenanceUnknown, bonferroniM)

	_, fpValues := collect(ids, byID, func(d queryDelta) *float64 { return d.FP10 })
	group.FP10 = groupMetric(name, "fp10", fpValues, opts, provenanceUnknown, bonferroniM)

	_, latValues := collect(ids, byID, func(d queryDelta) *float64 { return d.LatencyMs })
	group.Latency = LatencyMetric{N: len(latValues), MedianDelta: median(latValues)}

	group.WorstQueryIDs = worstByNDCG(ids, byID)
	return group
}

// collect returns the (id, delta-value) pairs among ids whose selector is
// non-nil, in the same order as ids (which callers already keep in the
// globally sorted query_id order — see Compare). Returning both keeps
// callers who need the surviving IDs (currently none directly, but kept for
// symmetry/tests) from having to re-derive them.
func collect(ids []string, byID map[string]queryDelta, pick func(queryDelta) *float64) ([]string, []float64) {
	outIDs := make([]string, 0, len(ids))
	outValues := make([]float64, 0, len(ids))
	for _, id := range ids {
		v := pick(byID[id])
		if v == nil {
			continue
		}
		outIDs = append(outIDs, id)
		outValues = append(outValues, *v)
	}
	return outIDs, outValues
}

// groupMetric runs the fixed-seed paired bootstrap for one (group, metric)
// pair. n < opts.MinN, or an unknown-provenance compare (AllowV1), forces
// VerdictInconclusive without touching the RNG — an inconclusive verdict
// must never depend on random draws to reach the same answer twice.
// bonferroniM > 0 additionally populates BonferroniSignificant from the
// same resampled distribution at a corrected alpha; pass 0 for the gating
// (overall) group, which never needs that computation.
func groupMetric(groupName, metricName string, values []float64, opts Options, provenanceUnknown bool, bonferroniM int) GroupMetric {
	n := len(values)
	metric := GroupMetric{N: n, MeanDelta: mean(values)}
	if n < opts.MinN || provenanceUnknown {
		metric.Verdict = VerdictInconclusive
		return metric
	}

	rng := rand.New(rand.NewPCG(
		subSeed(opts.Seed, groupName, metricName, "s1"),
		subSeed(opts.Seed, groupName, metricName, "s2"),
	))
	resampled := make([]float64, opts.Iterations)
	for it := 0; it < opts.Iterations; it++ {
		sum := 0.0
		for i := 0; i < n; i++ {
			sum += values[rng.IntN(n)]
		}
		resampled[it] = sum / float64(n)
	}
	sort.Float64s(resampled)
	metric.HasCI = true
	metric.CILow = percentile(resampled, 0.025)
	metric.CIHigh = percentile(resampled, 0.975)

	switch {
	case metric.CIHigh < 0:
		metric.Verdict = VerdictRegressed
	case metric.CILow > 0:
		metric.Verdict = VerdictImprovedCandidate
	default:
		metric.Verdict = VerdictNoChange
	}

	if bonferroniM > 0 {
		alpha := 0.05 / float64(bonferroniM)
		lo := percentile(resampled, alpha/2)
		hi := percentile(resampled, 1-alpha/2)
		sig := hi < 0 || lo > 0
		metric.BonferroniSignificant = &sig
	}
	return metric
}

// percentile returns the p-th percentile (0 <= p <= 1) of sorted, a slice
// already in ascending order, using linear interpolation between the two
// bracketing order statistics (the method NumPy calls "linear", also known
// as Hazen's or the R-7 estimator: rank = p*(n-1), then interpolate).
//
// The previous implementation, percentileIndices, took nearest-rank
// integer indices lo=int(0.025*n) and hi=int(0.975*n)-1. Truncating instead
// of interpolating pulls both bounds inward — at n=1000 that is index 24
// instead of the linear-interpolation rank 24.975, and index 973 instead of
// rank 973.025 — so the reported 95% CI is narrower than nominal, most
// visibly at low --iterations counts (see docs/evaluation-protocol.md's
// minimum-iterations note). See TestPercentile for the exact numbers at
// n=40 and n=100.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 || p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[n-1]
	}
	rank := p * float64(n-1)
	lo := int(rank)
	hi := lo + 1
	if hi >= n {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// worstByNDCG returns up to 3 query IDs with the most negative ndcg10 delta
// in the group (ties broken by ID for determinism) — IDs only, never a
// score alongside anything that could be mistaken for document content.
func worstByNDCG(ids []string, byID map[string]queryDelta) []string {
	type scored struct {
		id    string
		delta float64
	}
	var candidates []scored
	for _, id := range ids {
		if d := byID[id].NDCG10; d != nil {
			candidates = append(candidates, scored{id, *d})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].delta != candidates[j].delta {
			return candidates[i].delta < candidates[j].delta
		}
		return candidates[i].id < candidates[j].id
	})
	limit := 3
	if len(candidates) < limit {
		limit = len(candidates)
	}
	out := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, candidates[i].id)
	}
	return out
}

// subSeed derives a stable 64-bit seed from (base, parts...) so every
// (group, metric) pair gets its own independent, deterministic bootstrap
// stream: adding an unrelated slice or tag can never perturb another
// group's resampled sequence, and the same Options.Seed always reproduces
// the same Report byte-for-byte.
func subSeed(base uint64, parts ...string) uint64 {
	h := fnv.New64a()
	var buf [8]byte
	for i := 0; i < 8; i++ {
		buf[i] = byte(base >> (8 * i))
	}
	h.Write(buf[:])
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return h.Sum64()
}
