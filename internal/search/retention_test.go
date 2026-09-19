package search

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Retention-aware search (gmail segmentation pass: metadata.retention in
// {"keep", "low", "disposable"} for 11,965 tagged documents; SMS to follow).
//
// These tests sit at the same seam as insight_exclusion_test.go, for the same
// reason: the guard has to be visible to every caller of Service.Search
// (both /api/v1/search handlers, /api/v1/ask, the MCP search tool, the
// GraphQL resolver, the Discord gateway), not just to a handler that happens
// to remember to ask for it.
//
// Retention exclusion (applyRetentionExclusion) and the retention="low" score
// penalty (applyLowRetentionPenalty) are separate functions applied at
// separate points — see search.go's doc comments on each. Cases pinned here:
//  1. default exclude          — TestServiceSearch_ExcludesDisposableByDefault
//  2. explicit include          — TestServiceSearch_IncludeRetention_NotExcluded
//  3. untagged preserved        — TestApplyRetentionExclusion_UntaggedNeverExcluded,
//                                  TestApplyLowRetentionPenalty_UntaggedNeverPenalised
//  4. low-retention penalty     — TestApplyLowRetentionPenalty_Applied
//  5. penalty CHANGES RANK      — TestServiceSearch_LowRetentionPenalty_ReordersStoreOnlyResults,
//                                  TestServiceSearch_LowRetentionPenalty_ReordersAfterChunkFusion
// ---------------------------------------------------------------------------

// TestServiceSearch_ExcludesDisposableByDefault is the core wiring assertion:
// any caller that does not ask for disposable documents must have the
// exclusion applied before the store ever sees the query.
func TestServiceSearch_ExcludesDisposableByDefault(t *testing.T) {
	t.Parallel()

	docs := &recordingDocSearcher{}
	svc := NewService(docs, disabledEmbedder{})

	if _, err := svc.Search(context.Background(), model.SearchQuery{Query: "newsletter"}); err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if !containsRetention(docs.gotQuery.ExcludeRetention, model.RetentionDisposable) {
		t.Errorf("store received ExcludeRetention = %v, want it to contain %q — every caller of the service must inherit the retention guard",
			docs.gotQuery.ExcludeRetention, model.RetentionDisposable)
	}
}

// TestServiceSearch_IncludeRetention_NotExcluded verifies the single
// sanctioned way to opt out of the default exclusion still works.
func TestServiceSearch_IncludeRetention_NotExcluded(t *testing.T) {
	t.Parallel()

	docs := &recordingDocSearcher{}
	svc := NewService(docs, disabledEmbedder{})

	_, err := svc.Search(context.Background(), model.SearchQuery{Query: "newsletter", IncludeRetention: true})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if containsRetention(docs.gotQuery.ExcludeRetention, model.RetentionDisposable) {
		t.Errorf("store received ExcludeRetention = %v, want empty for an explicit include_retention=true request",
			docs.gotQuery.ExcludeRetention)
	}
}

// TestServiceSearch_AlreadyExcludedRetention_NoDuplicate verifies a caller who
// already excludes "disposable" does not get a duplicate entry (which would
// bind a two-element array to the SQL `<> ALL($n)` filter for no reason).
func TestServiceSearch_AlreadyExcludedRetention_NoDuplicate(t *testing.T) {
	t.Parallel()

	docs := &recordingDocSearcher{}
	svc := NewService(docs, disabledEmbedder{})

	_, err := svc.Search(context.Background(), model.SearchQuery{
		Query:            "newsletter",
		ExcludeRetention: []string{model.RetentionDisposable},
	})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if len(docs.gotQuery.ExcludeRetention) != 1 {
		t.Errorf("ExcludeRetention = %v, want exactly one entry", docs.gotQuery.ExcludeRetention)
	}
}

// TestApplyRetentionExclusion_UntaggedNeverExcluded is the "untagged
// preserved" case: a document with no "retention" key at all (the vast
// majority of the corpus) must pass through completely unchanged, including
// its Score, regardless of what ExcludeRetention contains.
func TestApplyRetentionExclusion_UntaggedNeverExcluded(t *testing.T) {
	t.Parallel()

	untagged := makeSearchResult(uuid.New(), "정상 문서", 0.42)
	q := model.SearchQuery{ExcludeRetention: []string{model.RetentionDisposable}}

	out := applyRetentionExclusion(q, []*model.SearchResult{untagged})

	if len(out) != 1 {
		t.Fatalf("results = %+v, want the untagged document preserved", out)
	}
	if out[0].Score != 0.42 {
		t.Errorf("Score = %v, want unchanged 0.42 for an untagged document", out[0].Score)
	}
}

// TestApplyRetentionExclusion_ExcludesDisposable is the fusion-level
// exclusion case: a disposable-tagged document named by ExcludeRetention is
// dropped.
func TestApplyRetentionExclusion_ExcludesDisposable(t *testing.T) {
	t.Parallel()

	disposable := makeSearchResult(uuid.New(), "광고 메일", 0.9)
	disposable.Metadata = map[string]any{"retention": model.RetentionDisposable}
	keep := makeSearchResult(uuid.New(), "중요 메일", 0.5)
	keep.Metadata = map[string]any{"retention": model.RetentionKeep}

	q := model.SearchQuery{ExcludeRetention: []string{model.RetentionDisposable}}
	out := applyRetentionExclusion(q, []*model.SearchResult{disposable, keep})

	if len(out) != 1 || out[0].ID != keep.ID {
		t.Errorf("results = %+v, want only the retention=keep document", out)
	}
}

// TestApplyLowRetentionPenalty_UntaggedNeverPenalised mirrors
// TestApplyRetentionExclusion_UntaggedNeverExcluded for the penalty function:
// a document with no retention tag must never have its Score touched.
func TestApplyLowRetentionPenalty_UntaggedNeverPenalised(t *testing.T) {
	t.Parallel()

	untagged := makeSearchResult(uuid.New(), "정상 문서", 0.42)

	out := applyLowRetentionPenalty(model.SearchQuery{}, []*model.SearchResult{untagged})

	if len(out) != 1 {
		t.Fatalf("results = %+v, want the untagged document preserved", out)
	}
	if out[0].Score != 0.42 {
		t.Errorf("Score = %v, want unchanged 0.42 for an untagged document", out[0].Score)
	}
}

// TestApplyLowRetentionPenalty_Applied is the "low penalty" case: a
// retention=low document's score is multiplied by
// model.LowRetentionPenalty, regardless of which lane it came from — this
// helper is the single point where every lane's fused result set is
// penalised.
func TestApplyLowRetentionPenalty_Applied(t *testing.T) {
	// Not t.Parallel(): mutates the shared SEARCH_LOW_RETENTION_PENALTY env var.
	t.Setenv("SEARCH_LOW_RETENTION_PENALTY", "0.25")

	low := makeSearchResult(uuid.New(), "뉴스레터", 1.0)
	low.Metadata = map[string]any{"retention": model.RetentionLow}

	out := applyLowRetentionPenalty(model.SearchQuery{}, []*model.SearchResult{low})

	if len(out) != 1 {
		t.Fatalf("results = %+v, want the low-retention document retained (penalised, not dropped)", out)
	}
	const want = 1.0 * 0.25
	if out[0].Score != want {
		t.Errorf("Score = %v, want %v (1.0 * SEARCH_LOW_RETENTION_PENALTY)", out[0].Score, want)
	}
}

// TestApplyLowRetentionPenalty_ReordersByScore is the pure-function proof
// that the penalty changes RANK, not just the Score field: given a low-tagged
// document ranked ABOVE a keep-tagged document, applying the penalty must
// re-sort the set so the keep document comes first once its score is no
// longer the smaller of the two.
func TestApplyLowRetentionPenalty_ReordersByScore(t *testing.T) {
	t.Setenv("SEARCH_LOW_RETENTION_PENALTY", "0.5")

	low := makeSearchResult(uuid.New(), "낮은 유지 문서", 0.9)
	low.Metadata = map[string]any{"retention": model.RetentionLow}
	keep := makeSearchResult(uuid.New(), "중요 문서", 0.5)
	keep.Metadata = map[string]any{"retention": model.RetentionKeep}

	// Input order mirrors the store's own ORDER BY score DESC: low (0.9)
	// ranked ahead of keep (0.5) BEFORE the penalty is considered.
	out := applyLowRetentionPenalty(model.SearchQuery{}, []*model.SearchResult{low, keep})

	if len(out) != 2 {
		t.Fatalf("results = %+v, want both documents retained", out)
	}
	if out[0].ID != keep.ID || out[1].ID != low.ID {
		t.Errorf("order = [%s, %s], want [keep, low] — the penalty (0.9*0.5=0.45 < 0.5) must flip the rank, not just the score",
			out[0].Title, out[1].Title)
	}
}

// TestApplyLowRetentionPenalty_PenaltyOneIsNoOp pins the documented escape
// hatch: SEARCH_LOW_RETENTION_PENALTY=1.0 must leave every score AND every
// position untouched.
func TestApplyLowRetentionPenalty_PenaltyOneIsNoOp(t *testing.T) {
	t.Setenv("SEARCH_LOW_RETENTION_PENALTY", "1.0")

	low := makeSearchResult(uuid.New(), "뉴스레터", 0.77)
	low.Metadata = map[string]any{"retention": model.RetentionLow}
	keep := makeSearchResult(uuid.New(), "중요 문서", 0.5)
	keep.Metadata = map[string]any{"retention": model.RetentionKeep}

	out := applyLowRetentionPenalty(model.SearchQuery{}, []*model.SearchResult{low, keep})

	if len(out) != 2 || out[0].ID != low.ID || out[1].ID != keep.ID {
		t.Errorf("results = %+v, want order and scores unchanged when the penalty is 1.0", out)
	}
	if out[0].Score != 0.77 {
		t.Errorf("Score = %v, want unchanged 0.77 when the penalty is 1.0", out[0].Score)
	}
}

// TestLowRetentionPenalty_InvalidEnvFallsBackToDefault covers the env-var
// parsing contract directly: unset/invalid/out-of-range values must degrade
// to model.DefaultLowRetentionPenalty rather than panicking or disabling the
// penalty silently.
func TestLowRetentionPenalty_InvalidEnvFallsBackToDefault(t *testing.T) {
	for _, v := range []string{"", "not-a-float", "1.5", "-0.1"} {
		v := v
		t.Run("env="+v, func(t *testing.T) {
			if v == "" {
				os.Unsetenv("SEARCH_LOW_RETENTION_PENALTY")
			} else {
				t.Setenv("SEARCH_LOW_RETENTION_PENALTY", v)
			}
			if got := model.LowRetentionPenalty(); got != model.DefaultLowRetentionPenalty {
				t.Errorf("LowRetentionPenalty() = %v, want default %v for env value %q",
					got, model.DefaultLowRetentionPenalty, v)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// End-to-end rank regression tests (Service.Search).
//
// These were written against the pre-fix code first, to prove the defect:
// applyRetentionFilters multiplied Score but never re-sorted, and mergeRRF
// computed each entry's fused Score from RANK POSITION alone — never from the
// pre-fusion Score a per-lane penalty had multiplied — so the "low" penalty
// never changed which document came first. Both tests below FAILED against
// that code (low ranked first in both cases) and PASS after moving the
// penalty to the single post-fusion point (applyLowRetentionPenalty) with a
// re-sort. See the task report for the exact FAIL output captured before the
// fix landed.
// ---------------------------------------------------------------------------

// TestServiceSearch_LowRetentionPenalty_ReordersStoreOnlyResults covers the
// store-only path: no chunk store is configured, so `results` is exactly what
// the store returned (already ordered by its own SQL `ORDER BY score DESC`,
// simulated here by recordingDocSearcher). The store put the low-tagged
// document first because its raw match score was higher; the penalty must
// still push it below the keep-tagged document once applied.
func TestServiceSearch_LowRetentionPenalty_ReordersStoreOnlyResults(t *testing.T) {
	t.Setenv("SEARCH_LOW_RETENTION_PENALTY", "0.5")

	low := makeSearchResult(uuid.New(), "낮은 유지 문서", 0.9)
	low.Metadata = map[string]any{"retention": model.RetentionLow}
	keep := makeSearchResult(uuid.New(), "중요 문서", 0.5)
	keep.Metadata = map[string]any{"retention": model.RetentionKeep}

	// Store order: low first (0.9), keep second (0.5) — the store scored the
	// low-retention document higher before retention was ever considered.
	docs := &recordingDocSearcher{results: []*model.SearchResult{low, keep}}
	svc := NewService(docs, disabledEmbedder{}) // no chunk store: store-only path

	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "q"})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want both documents retained (penalised, not dropped)", results)
	}
	if results[0].ID != keep.ID || results[1].ID != low.ID {
		t.Errorf("order = [%s, %s], want [keep, low] — the store-only path must re-sort after the penalty, not just multiply Score in place",
			results[0].Title, results[1].Title)
	}
}

// TestServiceSearch_LowRetentionPenalty_ReordersAfterChunkFusion covers the
// chunk-fusion path: an unrelated chunk-vector hit fills a spare slot so
// mergeRRF actually runs (chunkFused=true), which recomputes every entry's
// Score from RANK POSITION in its input list — discarding any pre-fusion
// per-lane Score multiplication entirely. The low document must still rank
// below keep in the final, fused-and-penalised result set.
func TestServiceSearch_LowRetentionPenalty_ReordersAfterChunkFusion(t *testing.T) {
	t.Setenv("SEARCH_LOW_RETENTION_PENALTY", "0.5")

	low := makeSearchResult(uuid.New(), "낮은 유지 문서", 0.9)
	low.Metadata = map[string]any{"retention": model.RetentionLow}
	keep := makeSearchResult(uuid.New(), "중요 문서", 0.5)
	keep.Metadata = map[string]any{"retention": model.RetentionKeep}

	// Store order mirrors the store-only test: low ranked ahead of keep.
	docs := &recordingDocSearcher{results: []*model.SearchResult{low, keep}}

	// An unrelated third document arrives only through the chunk-vector lane,
	// which is enough to make mergeRRF run (chunkFused=true) without touching
	// low/keep's relative fused score directly — isolating the "does the
	// fusion path re-sort after the penalty" question from "does the chunk
	// lane's own contribution get penalised" (already covered elsewhere).
	extraDocID := uuid.New()
	chunks := &mockChunkSearcher{vectorResults: []store.ChunkSearchResult{
		makeChunkResult(extraDocID, 0, 0.6, "관련 없는 문서"),
	}}

	svc := NewService(docs, stubEnabledEmbedder{}).WithChunkStore(chunks)

	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "q", Limit: 5})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %+v, want low, keep, and the chunk-only document all retained", results)
	}

	rank := make(map[uuid.UUID]int, len(results))
	for i, r := range results {
		rank[r.ID] = i
	}
	if rank[low.ID] <= rank[keep.ID] {
		t.Errorf("low.ID rank=%d, keep.ID rank=%d — want low ranked BELOW keep after fusion, "+
			"the penalty must survive mergeRRF's rank-based rescoring, not just multiply a pre-fusion Score that fusion discards",
			rank[low.ID], rank[keep.ID])
	}
}

// TestServiceSearch_LowRetentionPenalty_OverfetchPreventsMembershipLoss is the
// MEMBERSHIP test (as distinct from the pure re-ordering tests above): it
// proves that a keep-tagged document reachable ONLY through the chunk-vector
// lane is not permanently discarded by mergeRRF's own internal truncation
// before applyLowRetentionPenalty ever gets a chance to demote whatever
// displaced it.
//
// Setup: q.Limit=2. Primary (store) already fills both slots with
// [low, keep1] (low ranked first — the store scored it higher before
// retention was considered). A THIRD, keep-tagged document (keep2) is
// reachable only through the chunk-vector lane.
//
// Without overfetch, mergeRRF(primary, secondary, limit=q.Limit=2) computes
// remaining := 2 - len(primary) = 0, so keep2 is refused entry into the
// merged set outright — a fusion-time decision no downstream penalty can
// undo, since reordering an already-truncated list cannot resurrect a
// document that was never admitted. With laneLimit overfetched to
// overfetchLimit(2)=4, remaining = 4-2 = 2 > 0, so keep2 IS admitted; the
// single post-fusion point then demotes low below both keep-tagged
// documents, and Search()'s own trailing truncate to the real q.Limit=2
// keeps [keep2, keep1] — low is excluded from the final page entirely, not
// just re-ranked within it.
func TestServiceSearch_LowRetentionPenalty_OverfetchPreventsMembershipLoss(t *testing.T) {
	t.Setenv("SEARCH_LOW_RETENTION_PENALTY", "0.5")

	low := makeSearchResult(uuid.New(), "낮은 유지 문서", 0.9)
	low.Metadata = map[string]any{"retention": model.RetentionLow}
	keep1 := makeSearchResult(uuid.New(), "중요 문서 1", 0.5)
	keep1.Metadata = map[string]any{"retention": model.RetentionKeep}

	// Primary already fills both of q.Limit's slots: low ranked ahead of
	// keep1, exactly as the store-only tests above set up.
	docs := &recordingDocSearcher{results: []*model.SearchResult{low, keep1}}

	// keep2 is a THIRD document, reachable ONLY through the chunk-vector
	// lane — never present in primary at all.
	keep2ID := uuid.New()
	chunks := &mockChunkSearcher{vectorResults: []store.ChunkSearchResult{
		{
			Chunk:            store.Chunk{ID: 1, DocumentID: keep2ID, Content: "keep2 chunk content"},
			Score:            0.6,
			DocumentTitle:    "중요 문서 2",
			DocumentSource:   "test",
			DocumentStatus:   "active",
			DocumentMetadata: map[string]any{"retention": model.RetentionKeep},
		},
	}}

	svc := NewService(docs, stubEnabledEmbedder{}).WithChunkStore(chunks)

	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "q", Limit: 2})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want exactly 2 (q.Limit=2)", results)
	}

	seen := map[uuid.UUID]bool{}
	for _, r := range results {
		seen[r.ID] = true
	}
	if seen[low.ID] {
		t.Errorf("results = %+v, want the retention=low document %s excluded from the final page entirely",
			results, low.ID)
	}
	if !seen[keep1.ID] || !seen[keep2ID] {
		t.Errorf("results = %+v, want BOTH retention=keep documents (%s from the store, %s from the chunk-only lane) present — "+
			"without overfetch, keep2 is refused entry by mergeRRF's own truncation before the penalty ever runs",
			results, keep1.ID, keep2ID)
	}
}

// TestServiceSearch_LowRetentionPenalty_PenaltyOneLeavesOrderUnchanged pins
// the escape hatch end-to-end: with SEARCH_LOW_RETENTION_PENALTY=1.0, the
// store's own order must survive untouched.
func TestServiceSearch_LowRetentionPenalty_PenaltyOneLeavesOrderUnchanged(t *testing.T) {
	t.Setenv("SEARCH_LOW_RETENTION_PENALTY", "1.0")

	low := makeSearchResult(uuid.New(), "낮은 유지 문서", 0.9)
	low.Metadata = map[string]any{"retention": model.RetentionLow}
	keep := makeSearchResult(uuid.New(), "중요 문서", 0.5)
	keep.Metadata = map[string]any{"retention": model.RetentionKeep}

	docs := &recordingDocSearcher{results: []*model.SearchResult{low, keep}}
	svc := NewService(docs, disabledEmbedder{})

	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "q"})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 2 || results[0].ID != low.ID || results[1].ID != keep.ID {
		t.Errorf("order changed with penalty=1.0: got [%s, %s], want [low, keep] unchanged",
			results[0].Title, results[1].Title)
	}
}

// TestServiceSearch_LowRetentionPenalty_SortRecentIgnoresPenalty verifies
// that Sort="recent" is never overridden by the retention penalty: time order
// wins, even though the more recent document happens to be retention=low and
// numerically loses its Score advantage to the penalty.
func TestServiceSearch_LowRetentionPenalty_SortRecentIgnoresPenalty(t *testing.T) {
	t.Setenv("SEARCH_LOW_RETENTION_PENALTY", "0.5")

	now := time.Now()
	older := now.Add(-time.Hour)

	recentLow := makeSearchResult(uuid.New(), "최근 낮은 유지 문서", 0.9)
	recentLow.Metadata = map[string]any{"retention": model.RetentionLow}
	recentLow.OccurredAt = &now

	olderKeep := makeSearchResult(uuid.New(), "오래된 중요 문서", 0.5)
	olderKeep.Metadata = map[string]any{"retention": model.RetentionKeep}
	olderKeep.OccurredAt = &older

	// Store order simulates SQL ORDER BY COALESCE(occurred_at, collected_at)
	// DESC: most recent (recentLow) first — this is the order the store-only
	// path never touches once Sort="recent" is set.
	docs := &recordingDocSearcher{results: []*model.SearchResult{recentLow, olderKeep}}
	svc := NewService(docs, disabledEmbedder{})

	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "q", Sort: model.SortRecent})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 2 || results[0].ID != recentLow.ID || results[1].ID != olderKeep.ID {
		t.Errorf("order = [%s, %s], want [recentLow, olderKeep] — Sort=recent must keep time order despite the penalty",
			results[0].Title, results[1].Title)
	}
}

func containsRetention(list []string, want string) bool {
	for _, r := range list {
		if r == want {
			return true
		}
	}
	return false
}
