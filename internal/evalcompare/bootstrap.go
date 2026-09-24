package evalcompare

import (
	"hash/fnv"
	"math/rand/v2"
	"sort"
)

// buildGroup computes the full comparison for one slice: n/mean/CI/verdict
// for ndcg10, recall10 and fp10, the median-only latency summary, and the
// worst (most regressed) query IDs by ndcg10 delta.
func buildGroup(name string, ids []string, byID map[string]queryDelta, opts Options, provenanceUnknown bool) Group {
	group := Group{Name: name}

	_, ndcgValues := collect(ids, byID, func(d queryDelta) *float64 { return d.NDCG10 })
	group.NDCG10 = groupMetric(name, "ndcg10", ndcgValues, opts, provenanceUnknown)

	_, recallValues := collect(ids, byID, func(d queryDelta) *float64 { return d.Recall10 })
	group.Recall10 = groupMetric(name, "recall10", recallValues, opts, provenanceUnknown)

	_, fpValues := collect(ids, byID, func(d queryDelta) *float64 { return d.FP10 })
	group.FP10 = groupMetric(name, "fp10", fpValues, opts, provenanceUnknown)

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
func groupMetric(groupName, metricName string, values []float64, opts Options, provenanceUnknown bool) GroupMetric {
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
	lo, hi := percentileIndices(len(resampled))
	metric.HasCI = true
	metric.CILow, metric.CIHigh = resampled[lo], resampled[hi]

	switch {
	case metric.CIHigh < 0:
		metric.Verdict = VerdictRegressed
	case metric.CILow > 0:
		metric.Verdict = VerdictImprovedCandidate
	default:
		metric.Verdict = VerdictNoChange
	}
	return metric
}

// percentileIndices returns the nearest-rank indices for a 95% CI (2.5th and
// 97.5th percentile) into a sorted slice of length n. n is always
// opts.Iterations, which normalized() guarantees is >= 1.
func percentileIndices(n int) (lo, hi int) {
	lo = int(0.025 * float64(n))
	hi = int(0.975*float64(n)) - 1
	if hi >= n {
		hi = n - 1
	}
	if hi < 0 {
		hi = 0
	}
	if lo < 0 {
		lo = 0
	}
	if lo > hi {
		lo = hi
	}
	return lo, hi
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
