package search

import (
	"context"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// #215 — a chunk-only hit used to arrive with no event time.
//
// chunkVecToSearchResult / chunkToSearchResult built a model.Document out of
// the chunk join, and that join selected no timestamps. So every document
// reachable ONLY through a chunk lane came back with OccurredAt == nil and a
// zero CollectedAt, which recencyKey reads as "no usable timestamp" and
// sortByRecency puts LAST in both directions.
//
// The set was still correct — verifyWindow joins the same candidates back to
// `documents` and drops anything outside the window — but the ORDER was not.
// The visible symptom is a forward-looking /ask ("다음 주 일정") whose most
// imminent entry happens to be a chunk-only hit: it is shown last, i.e. as the
// FURTHEST away, which is the exact inversion the ascending branch exists to
// prevent.
//
// The fix is one JOIN column away: the chunk queries already join `documents`,
// so occurred_at/collected_at cost nothing extra to select.
// ---------------------------------------------------------------------------

// chunkResultAt builds a chunk-lane row that carries its parent document's
// timestamps, which is what the chunk join now selects.
func chunkResultAt(id uuid.UUID, src model.SourceType, score float64, occurred *time.Time, collected time.Time) store.ChunkSearchResult {
	r := chunkResult(id, src, score)
	r.DocumentOccurredAt = occurred
	r.DocumentCollectedAt = collected
	return r
}

// TestChunkConversion_CarriesDocumentTimestamps pins the conversion itself:
// both chunk lanes must copy the parent document's event and ingest times onto
// the SearchResult. Without this the sort has nothing to work with no matter
// what the SQL selects.
func TestChunkConversion_CarriesDocumentTimestamps(t *testing.T) {
	t.Parallel()

	occurred := time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC)
	collected := time.Date(2026, 8, 19, 1, 0, 0, 0, time.UTC)
	id := uuid.New()
	row := chunkResultAt(id, model.SourceCalendar, 0.9, &occurred, collected)

	cases := []struct {
		name    string
		convert func(store.ChunkSearchResult) *model.SearchResult
	}{
		{"chunk-vector", chunkVecToSearchResult},
		{"chunk-fts", chunkToSearchResult},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tc.convert(row)
			if got.OccurredAt == nil {
				t.Fatalf("OccurredAt = nil, want %s — a chunk-only hit with no event time cannot be ordered", occurred)
			}
			if !got.OccurredAt.Equal(occurred) {
				t.Errorf("OccurredAt = %s, want %s", got.OccurredAt, occurred)
			}
			if !got.CollectedAt.Equal(collected) {
				t.Errorf("CollectedAt = %s, want %s", got.CollectedAt, collected)
			}
		})
	}
}

// TestChunkConversion_NilOccurredAtStaysNil guards the other half of the
// contract: a document with no event-time concept must NOT acquire one. The
// pointer is copied, not synthesised from collected_at — recencyKey's
// descending branch does that COALESCE itself, and doing it here as well would
// make "happened then" and "ingested then" indistinguishable downstream.
func TestChunkConversion_NilOccurredAtStaysNil(t *testing.T) {
	t.Parallel()

	collected := time.Date(2026, 8, 19, 1, 0, 0, 0, time.UTC)
	row := chunkResultAt(uuid.New(), model.SourceSlack, 0.9, nil, collected)

	for _, convert := range []func(store.ChunkSearchResult) *model.SearchResult{
		chunkVecToSearchResult, chunkToSearchResult,
	} {
		got := convert(row)
		if got.OccurredAt != nil {
			t.Errorf("OccurredAt = %s, want nil — collected_at must not be promoted to an event time", got.OccurredAt)
		}
		if !got.CollectedAt.Equal(collected) {
			t.Errorf("CollectedAt = %s, want %s", got.CollectedAt, collected)
		}
	}
}

// TestSearch_SortRecent_FutureWindow_ChunkOnlyHit_RanksFirst is the issue's
// headline symptom.
//
// The chunk-only document is the SOONEST of the three. Under `occurred_at ASC`
// it belongs at position 0. Before the fix it was placed last, so the answer to
// "다음 주 일정" opened with the furthest-away entry and closed with the most
// imminent one.
func TestSearch_SortRecent_FutureWindow_ChunkOnlyHit_RanksFirst(t *testing.T) {
	t.Parallel()

	now := time.Now()
	from, to := now.Add(1*time.Hour), now.Add(30*24*time.Hour)

	// Store hits: both later than the chunk-only hit below.
	mid := recentResult(uuid.New(), "모레 회의", now.Add(48*time.Hour), 0.9)
	late := recentResult(uuid.New(), "다음 주 회의", now.Add(120*time.Hour), 0.5)

	// Chunk-only: the most imminent entry of the three.
	soonestID := uuid.New()
	soonestAt := now.Add(6 * time.Hour)

	docs := &recordingDocSearcher{results: []*model.SearchResult{mid, late}}
	chunks := &mockChunkSearcher{vectorResults: []store.ChunkSearchResult{
		chunkResultAt(soonestID, model.SourceCalendar, 0.99, &soonestAt, now),
	}}
	checker := &fakeOccurredChecker{occurredAt: map[uuid.UUID]time.Time{
		mid.ID:    now.Add(48 * time.Hour),
		late.ID:   now.Add(120 * time.Hour),
		soonestID: soonestAt,
	}}
	svc := NewService(docs, stubEnabledEmbedder{}).
		WithChunkStore(chunks).
		WithOccurredRangeChecker(checker)

	results, err := svc.Search(context.Background(), model.SearchQuery{
		Query:        "다음 주 일정",
		Limit:        20,
		Sort:         "recent",
		OccurredFrom: &from,
		OccurredTo:   &to,
	})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if !sameIDs(results, soonestID, mid.ID, late.ID) {
		t.Fatalf("order = %v, want soonest-first [%s %s %s] — the chunk-only hit is the most imminent entry and must lead",
			idsOf(results), soonestID, mid.ID, late.ID)
	}
}

// TestSearch_SortRecent_PastWindow_ChunkOnlyHit_RanksByTime is the descending
// counterpart: the chunk-only hit lands in the MIDDLE, which is only
// observable once it carries a real timestamp. "Last" was previously
// indistinguishable from "correct" for any chunk-only hit that happened to be
// the oldest, so the past direction gets a case where last is wrong.
func TestSearch_SortRecent_PastWindow_ChunkOnlyHit_RanksByTime(t *testing.T) {
	t.Parallel()

	now := time.Now()
	from, to := now.Add(-30*24*time.Hour), now.Add(-time.Hour)

	newest := recentResult(uuid.New(), "어제 통화", now.Add(-2*time.Hour), 0.9)
	oldest := recentResult(uuid.New(), "지난달 통화", now.Add(-240*time.Hour), 0.5)

	middleID := uuid.New()
	middleAt := now.Add(-48 * time.Hour)

	docs := &recordingDocSearcher{results: []*model.SearchResult{newest, oldest}}
	chunks := &mockChunkSearcher{vectorResults: []store.ChunkSearchResult{
		chunkResultAt(middleID, model.SourceCallTranscript, 0.99, &middleAt, now),
	}}
	checker := &fakeOccurredChecker{occurredAt: map[uuid.UUID]time.Time{
		newest.ID: now.Add(-2 * time.Hour),
		oldest.ID: now.Add(-240 * time.Hour),
		middleID:  middleAt,
	}}
	svc := NewService(docs, stubEnabledEmbedder{}).
		WithChunkStore(chunks).
		WithOccurredRangeChecker(checker)

	results, err := svc.Search(context.Background(), model.SearchQuery{
		Query:        "통화",
		Limit:        20,
		Sort:         "recent",
		OccurredFrom: &from,
		OccurredTo:   &to,
	})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if !sameIDs(results, newest.ID, middleID, oldest.ID) {
		t.Fatalf("order = %v, want newest-first [%s %s %s] — the chunk-only hit belongs between the two store hits",
			idsOf(results), newest.ID, middleID, oldest.ID)
	}
}

// TestSearch_SortRecent_NoWindow_ChunkOnlyHit_UsesCollectedAt covers the
// descending branch's COALESCE: with no window at all the chunk lanes run
// unverified, and a chunk-only document whose parent has no occurred_at must
// still be ordered — on collected_at, exactly as the store's
// COALESCE(occurred_at, collected_at) DESC does for its own rows.
func TestSearch_SortRecent_NoWindow_ChunkOnlyHit_UsesCollectedAt(t *testing.T) {
	t.Parallel()

	now := time.Now()

	newest := recentResult(uuid.New(), "최근 문서", now.Add(-2*time.Hour), 0.9)
	oldest := recentResult(uuid.New(), "오래된 문서", now.Add(-240*time.Hour), 0.5)

	middleID := uuid.New()
	middleCollected := now.Add(-48 * time.Hour)

	docs := &recordingDocSearcher{results: []*model.SearchResult{newest, oldest}}
	chunks := &mockChunkSearcher{vectorResults: []store.ChunkSearchResult{
		chunkResultAt(middleID, model.SourceLLMMemory, 0.99, nil, middleCollected),
	}}
	svc := NewService(docs, stubEnabledEmbedder{}).WithChunkStore(chunks)

	results, err := svc.Search(context.Background(), model.SearchQuery{
		Query: "문서",
		Limit: 20,
		Sort:  "recent",
	})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if !sameIDs(results, newest.ID, middleID, oldest.ID) {
		t.Fatalf("order = %v, want [%s %s %s] — a chunk-only hit with no event time degrades to ingest order, not to last",
			idsOf(results), newest.ID, middleID, oldest.ID)
	}
}
