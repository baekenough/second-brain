package search

import (
	"sort"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// sortByRecency re-establishes the caller's Sort="recent" order over a result
// set this service assembled itself.
//
// Why it has to exist at all: the store applies the order in SQL, but only to
// the rows IT selected. Once mergeRRF folds chunk-lane candidates into that
// list it re-sorts everything by fused score, which discards the store's ORDER
// BY entirely — so "recent" held only for requests where the chunk lanes
// happened to contribute nothing. The split this restores is:
//
//	RRF decides WHICH documents come back; Sort decides IN WHICH ORDER.
//
// ascending comes from model.SearchQuery.RecencyAscending — the same function
// the store's ORDER BY is derived from. It is passed in rather than recomputed
// here so this file cannot grow a second copy of the rule.
//
// The comparison is a TOTAL order (time, then score, then id), not a stable
// sort over an arbitrary input: mergeRRF flattens a map, so ties left to the
// input order would resolve differently on every run — the same class of
// nondeterminism as an unordered LIMIT (issue #191). Score before id keeps the
// tie-break useful (equal-instant documents stay in relevance order) and id
// makes it total, since two results can share both an instant and a score.
func sortByRecency(results []*model.SearchResult, ascending bool) {
	sort.SliceStable(results, func(i, j int) bool {
		ki, oki := recencyKey(results[i], ascending)
		kj, okj := recencyKey(results[j], ascending)

		// A result with no usable timestamp cannot be placed among results
		// that have one, in EITHER direction, so it goes last rather than
		// being treated as the zero instant (which would put it first under
		// ASC — the reverse of what "no known time" should mean).
		if oki != okj {
			return oki
		}
		if oki && !ki.Equal(kj) {
			if ascending {
				return ki.Before(kj)
			}
			return kj.Before(ki)
		}
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].ID.String() < results[j].ID.String()
	})
}

// recencyKey returns the instant a result is ordered on, and whether it has
// one. The two branches mirror store.sortOrder's two clauses exactly:
//
//   - ascending (window entirely in the future): occurred_at alone, no COALESCE.
//     Whenever either bound is set the store's lanes already exclude
//     occurred_at IS NULL, so on this branch a missing event time means the row
//     did not come through those lanes — coalescing onto collected_at would
//     order it by when it was INGESTED, mixed in among rows ordered by when
//     they will HAPPEN.
//   - descending: COALESCE(occurred_at, collected_at), so a document with no
//     event-time concept degrades to ingest order exactly as it does in SQL.
//
// Chunk-lane rows are ordinary inputs here. They used to be the whole reason
// the "no usable timestamp" branch fired: the chunk join selected no
// timestamps, so every chunk-only document went last regardless of when it
// happened (#215). The join now carries the parent's occurred_at/collected_at
// and chunkVecToSearchResult / chunkToSearchResult copy them across, so a
// chunk-only hit is placed on the same key as a store hit. The branch below is
// therefore reached only by a document that genuinely has no event time, on the
// ascending side — which is what it was written for.
func recencyKey(r *model.SearchResult, ascending bool) (time.Time, bool) {
	if r.OccurredAt != nil && !r.OccurredAt.IsZero() {
		return *r.OccurredAt, true
	}
	if ascending {
		return time.Time{}, false
	}
	if r.CollectedAt.IsZero() {
		return time.Time{}, false
	}
	return r.CollectedAt, true
}
