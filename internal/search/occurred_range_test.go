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
// Event-time window at the service seam.
//
// The store applies model.SearchQuery's occurred_at window as a WHERE predicate
// in every one of its lanes. The service's two CHUNK lanes cannot: ChunkSearcher
// takes only (vector|text, limit), and the rows it returns carry no occurred_at
// at all (see chunkVecToSearchResult / chunkToSearchResult — they populate ID,
// source, title, status and nothing else). So a windowed query that reaches
// those lanes gets documents from outside the window, and there is no field on
// the result to post-filter them by.
//
// That is not a cosmetic leak. mergeRRF fills the slots the primary lane left
// empty — and a narrow window is precisely the case where the primary lane
// returns two or three documents out of twenty. The chunk-FTS fallback is worse:
// it fires exactly when the window matched nothing and replaces the honest empty
// answer with unfiltered matches, i.e. it silently widens the window the user
// asked for.
//
// These tests pin the only defensible behaviour available without a schema
// change: when a window is set, the chunk lanes are skipped entirely.
// ---------------------------------------------------------------------------

var (
	svcWindowFrom = time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	svcWindowTo   = time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
)

func windowedQuery() model.SearchQuery {
	return model.SearchQuery{
		Query:        "오늘 일정에 대해 알려줘",
		Limit:        20,
		OccurredFrom: &svcWindowFrom,
		OccurredTo:   &svcWindowTo,
	}
}

// TestServiceSearch_PassesWindowToStore is the wiring assertion: the store is
// the only component that can honour the window, so it has to receive it
// unmodified.
func TestServiceSearch_PassesWindowToStore(t *testing.T) {
	t.Parallel()

	docs := &recordingDocSearcher{}
	svc := NewService(docs, disabledEmbedder{})

	if _, err := svc.Search(context.Background(), windowedQuery()); err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	got := docs.gotQuery
	if got.OccurredFrom == nil || !got.OccurredFrom.Equal(svcWindowFrom) {
		t.Errorf("store received OccurredFrom = %v, want %s", got.OccurredFrom, svcWindowFrom)
	}
	if got.OccurredTo == nil || !got.OccurredTo.Equal(svcWindowTo) {
		t.Errorf("store received OccurredTo = %v, want %s", got.OccurredTo, svcWindowTo)
	}
}

// TestServiceSearch_WindowedQuery_ChunkVectorLaneDoesNotFill pins that an
// unfiltered chunk hit cannot take one of the slots the windowed primary lane
// left empty.
func TestServiceSearch_WindowedQuery_ChunkVectorLaneDoesNotFill(t *testing.T) {
	t.Parallel()

	inWindowID := uuid.New()
	outOfWindowID := uuid.New()

	docs := &recordingDocSearcher{results: []*model.SearchResult{
		makeSearchResult(inWindowID, "in-window calendar entry", 0.5),
	}}
	chunks := &mockChunkSearcher{vectorResults: []store.ChunkSearchResult{
		{
			Chunk:          store.Chunk{ID: 1, DocumentID: outOfWindowID, Content: "some message from last year"},
			Score:          0.99,
			DocumentSource: string(model.SourceSMS),
			DocumentStatus: "active",
		},
	}}

	svc := NewService(docs, stubEnabledEmbedder{}).WithChunkStore(chunks)
	results, err := svc.Search(context.Background(), windowedQuery())
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	for _, r := range results {
		if r.ID == outOfWindowID {
			t.Fatalf("chunk vector lane filled a slot with a document the window cannot vouch for (id %s): %+v", r.ID, results)
		}
	}
	if len(results) != 1 || results[0].ID != inWindowID {
		t.Errorf("results = %+v, want only the store's in-window document %s", results, inWindowID)
	}
}

// TestServiceSearch_WindowedQuery_EmptyPrimary_NoChunkFTSFallback pins the
// honesty rule: a window that matches nothing must answer "nothing", not fall
// back to a lane that ignores the window.
func TestServiceSearch_WindowedQuery_EmptyPrimary_NoChunkFTSFallback(t *testing.T) {
	t.Parallel()

	docs := &recordingDocSearcher{} // window matched nothing
	chunks := &mockChunkSearcher{ftsResults: []store.ChunkSearchResult{
		{
			Chunk:          store.Chunk{ID: 1, DocumentID: uuid.New(), Content: "a message from some other day"},
			Rank:           0.8,
			DocumentSource: string(model.SourceSMS),
			DocumentStatus: "active",
		},
	}}

	svc := NewService(docs, disabledEmbedder{}).WithChunkStore(chunks)
	results, err := svc.Search(context.Background(), windowedQuery())
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if len(results) != 0 {
		t.Errorf("windowed query with no in-window documents returned %+v, want an empty result — widening the window silently is worse than an empty answer", results)
	}
}

// TestServiceSearch_OneSidedWindow_SkipsChunkLanes covers the half-bounded
// window; only one bound being set is still a window.
func TestServiceSearch_OneSidedWindow_SkipsChunkLanes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		query model.SearchQuery
	}{
		{"from only", model.SearchQuery{Query: "q", Limit: 20, OccurredFrom: &svcWindowFrom}},
		{"to only", model.SearchQuery{Query: "q", Limit: 20, OccurredTo: &svcWindowTo}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docs := &recordingDocSearcher{}
			chunks := &mockChunkSearcher{ftsResults: []store.ChunkSearchResult{
				{
					Chunk:          store.Chunk{ID: 1, DocumentID: uuid.New(), Content: "out of window"},
					Rank:           0.8,
					DocumentSource: string(model.SourceSMS),
					DocumentStatus: "active",
				},
			}}
			svc := NewService(docs, disabledEmbedder{}).WithChunkStore(chunks)

			results, err := svc.Search(context.Background(), tc.query)
			if err != nil {
				t.Fatalf("Search returned error: %v", err)
			}
			if len(results) != 0 {
				t.Errorf("results = %+v, want empty — a one-sided window is still a window", results)
			}
		})
	}
}

// TestServiceSearch_NoWindow_ChunkLanesStillRun is the control that keeps the
// guard narrow. Every non-temporal query — the overwhelming majority — must
// keep both chunk lanes, or this fix would quietly delete a retrieval signal
// from the whole system.
func TestServiceSearch_NoWindow_ChunkLanesStillRun(t *testing.T) {
	t.Parallel()

	chunkDocID := uuid.New()

	// Chunk-FTS fallback path: primary empty, no window.
	docs := &recordingDocSearcher{}
	chunks := &mockChunkSearcher{ftsResults: []store.ChunkSearchResult{
		{
			Chunk:          store.Chunk{ID: 1, DocumentID: chunkDocID, Content: "a chunk hit"},
			Rank:           0.8,
			DocumentSource: string(model.SourceSMS),
			DocumentStatus: "active",
		},
	}}
	svc := NewService(docs, disabledEmbedder{}).WithChunkStore(chunks)

	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "budget", Limit: 20})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 1 || results[0].ID != chunkDocID {
		t.Fatalf("results = %+v, want the chunk-FTS fallback hit %s", results, chunkDocID)
	}

	// Chunk-vector merge path: primary non-empty, no window.
	primaryID := uuid.New()
	docs2 := &recordingDocSearcher{results: []*model.SearchResult{makeSearchResult(primaryID, "primary", 0.5)}}
	chunks2 := &mockChunkSearcher{vectorResults: []store.ChunkSearchResult{
		{
			Chunk:          store.Chunk{ID: 2, DocumentID: chunkDocID, Content: "a chunk hit"},
			Score:          0.99,
			DocumentSource: string(model.SourceSMS),
			DocumentStatus: "active",
		},
	}}
	svc2 := NewService(docs2, stubEnabledEmbedder{}).WithChunkStore(chunks2)

	results2, err := svc2.Search(context.Background(), model.SearchQuery{Query: "budget", Limit: 20})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	var sawChunkDoc bool
	for _, r := range results2 {
		if r.ID == chunkDocID {
			sawChunkDoc = true
		}
	}
	if !sawChunkDoc {
		t.Errorf("results = %+v, want the chunk-vector document %s to still fill a free slot when no window is set", results2, chunkDocID)
	}
}
