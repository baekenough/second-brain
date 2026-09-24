package evalcompare

import (
	"sort"

	"github.com/baekenough/second-brain/internal/evaldump"
)

// queryDelta is (candidate - baseline) for one paired query. A nil field
// means that metric could not be compared for this query on at least one
// side — most commonly ndcg10/recall10 being nil because the query has no
// positive labels. fp10 and latency are present on every v2 row, so those
// two fields are non-nil whenever the row parsed at all.
type queryDelta struct {
	ID        string
	NDCG10    *float64
	Recall10  *float64
	FP10      *float64
	LatencyMs *float64
}

// buildDeltas computes one queryDelta per id, in the given order. ids must
// already be validated as present in both maps (see pairedIDs).
func buildDeltas(ids []string, base, cand map[string]evaldump.Row) []queryDelta {
	out := make([]queryDelta, 0, len(ids))
	for _, id := range ids {
		b, c := base[id], cand[id]
		d := queryDelta{ID: id}
		if b.NDCG10 != nil && c.NDCG10 != nil {
			v := *c.NDCG10 - *b.NDCG10
			d.NDCG10 = &v
		}
		if b.Recall10 != nil && c.Recall10 != nil {
			v := *c.Recall10 - *b.Recall10
			d.Recall10 = &v
		}
		if b.FP10 != nil && c.FP10 != nil {
			v := float64(*c.FP10 - *b.FP10)
			d.FP10 = &v
		}
		if b.LatencyMs != nil && c.LatencyMs != nil {
			v := *c.LatencyMs - *b.LatencyMs
			d.LatencyMs = &v
		}
		out = append(out, d)
	}
	return out
}

func deltasByID(deltas []queryDelta) map[string]queryDelta {
	out := make(map[string]queryDelta, len(deltas))
	for _, d := range deltas {
		out[d.ID] = d
	}
	return out
}

// buildGroups assigns every paired query to "overall" plus zero or more
// derived/manual slices, keyed by group name. Membership comes from the
// label side of the data only (the ground truth attached to the label
// document, or a manually-reviewed tag) — never from which documents a run
// happened to return, per the #269 design ("결과 top-K 의 소스 분포를 정답
// 그룹 라벨로 대체 사용하지 않는다"). Source of truth for answer_source and
// query_source is the baseline row; both are label-side facts that do not
// change between baseline and candidate runs of the same label set.
func buildGroups(ids []string, base map[string]evaldump.Row, slices *Slices) map[string][]string {
	out := map[string][]string{GroupOverall: append([]string(nil), ids...)}
	add := func(group, id string) { out[group] = append(out[group], id) }

	for _, id := range ids {
		row := base[id]
		seenSource := map[string]bool{}
		for _, doc := range row.RelevantDocs {
			if doc.SourceType == "" || seenSource[doc.SourceType] {
				continue
			}
			seenSource[doc.SourceType] = true
			add(prefixAnswerSource+doc.SourceType, id)
		}
		if row.QuerySource != "" {
			add(prefixQuerySource+row.QuerySource, id)
		}
		if slices != nil {
			for _, tag := range slices.Tags[id] {
				add(prefixTag+tag, id)
			}
		}
	}
	return out
}

// validateSlices rejects a slices file that names a query_id outside the
// compared set — a stale or mistyped file must fail loudly (InvalidError),
// not silently label nothing.
func validateSlices(s *Slices, ids []string) error {
	known := make(map[string]bool, len(ids))
	for _, id := range ids {
		known[id] = true
	}
	var unknown []string
	for id := range s.Tags {
		if !known[id] {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return invalid("slices file references %d unknown query id(s) not present in the compared dumps, e.g. %s",
		len(unknown), unknown[0])
}
