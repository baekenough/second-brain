package search

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// R4 — a window used to switch the chunk lanes off entirely.
//
// The original gate (`windowed := q.OccurredFrom != nil || q.OccurredTo != nil`
// → skip both chunk lanes) was correct but blunt: chunk rows carry no
// occurred_at, so there was nothing to post-filter them by, and admitting them
// would have let out-of-window documents take the slots the correctly-narrowed
// store result left empty.
//
// The cost of that gate scales with how often a window is set. A query planner
// sets one on every temporal question, so chunk-level recall would disappear
// for a large and growing share of traffic.
//
// The fix keeps the constraint and drops the blanket skip: chunk candidates are
// verified against the SAME window by joining them back to `documents` on
// document id (OccurredRangeChecker). Rows whose occurred_at falls outside the
// window — and rows whose occurred_at is NULL, which the SQL comparison
// excludes for free — do not survive.
//
// Fail-closed in two places:
//   - no checker configured  → the old skip still applies (never admit
//     unverifiable candidates),
//   - checker returns an error → chunk candidates are dropped for that request.
// ---------------------------------------------------------------------------

// fakeOccurredChecker verifies candidate IDs against an in-memory map of
// occurred_at values, reproducing the store's semantics: half-open [from, to),
// and a document with no occurred_at never passes a window.
type fakeOccurredChecker struct {
	occurredAt map[uuid.UUID]time.Time
	err        error
	calls      int
	gotIDs     []uuid.UUID
}

func (f *fakeOccurredChecker) FilterIDsByOccurredRange(_ context.Context, ids []uuid.UUID, from, to *time.Time) (map[uuid.UUID]struct{}, error) {
	f.calls++
	f.gotIDs = append(f.gotIDs, ids...)
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		at, ok := f.occurredAt[id]
		if !ok {
			continue // NULL occurred_at: excluded whenever a bound is set
		}
		if from != nil && at.Before(*from) {
			continue
		}
		if to != nil && !at.Before(*to) {
			continue
		}
		out[id] = struct{}{}
	}
	return out, nil
}

func inWindowTime() time.Time { return svcWindowFrom.Add(2 * time.Hour) }
func outOfWindowTime() time.Time {
	return svcWindowFrom.Add(-72 * time.Hour)
}

// TestSearch_Windowed_ChunkVectorLane_RunsAndIsVerified is the recall half of
// R4: an in-window chunk hit now reaches the caller instead of being discarded
// with the whole lane.
func TestSearch_Windowed_ChunkVectorLane_RunsAndIsVerified(t *testing.T) {
	t.Parallel()

	storeDoc := uuid.New()
	inWindow := uuid.New()
	outOfWindow := uuid.New()
	unknownTime := uuid.New() // occurred_at IS NULL

	docs := &recordingDocSearcher{results: []*model.SearchResult{
		makeSearchResult(storeDoc, "in-window calendar entry", 0.5),
	}}
	chunks := &mockChunkSearcher{vectorResults: []store.ChunkSearchResult{
		chunkResult(inWindow, model.SourceSMS, 0.99),
		chunkResult(outOfWindow, model.SourceSMS, 0.98),
		chunkResult(unknownTime, model.SourceSMS, 0.97),
	}}
	checker := &fakeOccurredChecker{occurredAt: map[uuid.UUID]time.Time{
		inWindow:    inWindowTime(),
		outOfWindow: outOfWindowTime(),
	}}

	svc := NewService(docs, stubEnabledEmbedder{}).
		WithChunkStore(chunks).
		WithOccurredRangeChecker(checker)

	results, err := svc.Search(context.Background(), windowedQuery())
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	got := map[uuid.UUID]bool{}
	for _, r := range results {
		got[r.ID] = true
	}
	if !got[inWindow] {
		t.Errorf("results = %+v, want the verified in-window chunk hit %s — the point of R4 is that it is no longer discarded", results, inWindow)
	}
	if got[outOfWindow] {
		t.Errorf("out-of-window chunk hit %s survived verification", outOfWindow)
	}
	if got[unknownTime] {
		t.Errorf("chunk hit %s with no occurred_at survived a windowed query", unknownTime)
	}
	if !got[storeDoc] {
		t.Errorf("results = %+v, want the store's own result preserved", results)
	}
	if checker.calls == 0 {
		t.Error("chunk candidates were admitted without any occurred_at verification")
	}
}

// TestSearch_Windowed_ChunkFTSFallback_RunsAndIsVerified covers the fallback
// lane. It fires when the store found nothing — the case where an unverified
// lane would silently widen the window the caller asked for.
func TestSearch_Windowed_ChunkFTSFallback_RunsAndIsVerified(t *testing.T) {
	t.Parallel()

	inWindow, outOfWindow := uuid.New(), uuid.New()

	docs := &recordingDocSearcher{} // window matched nothing in the store
	chunks := &mockChunkSearcher{ftsResults: []store.ChunkSearchResult{
		chunkResult(outOfWindow, model.SourceSMS, 0.9),
		chunkResult(inWindow, model.SourceSMS, 0.8),
	}}
	checker := &fakeOccurredChecker{occurredAt: map[uuid.UUID]time.Time{
		inWindow:    inWindowTime(),
		outOfWindow: outOfWindowTime(),
	}}

	svc := NewService(docs, disabledEmbedder{}).
		WithChunkStore(chunks).
		WithOccurredRangeChecker(checker)

	results, err := svc.Search(context.Background(), windowedQuery())
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 1 || results[0].ID != inWindow {
		t.Fatalf("results = %+v, want only the verified in-window chunk %s", results, inWindow)
	}
}

// TestSearch_Windowed_CheckerError_DropsChunkCandidates: verification failure
// must not degrade into "admit everything". An unverifiable candidate under a
// window is indistinguishable from an out-of-window one.
func TestSearch_Windowed_CheckerError_DropsChunkCandidates(t *testing.T) {
	t.Parallel()

	chunkDoc := uuid.New()
	docs := &recordingDocSearcher{}
	chunks := &mockChunkSearcher{ftsResults: []store.ChunkSearchResult{
		chunkResult(chunkDoc, model.SourceSMS, 0.9),
	}}
	checker := &fakeOccurredChecker{err: errors.New("connection refused")}

	svc := NewService(docs, disabledEmbedder{}).
		WithChunkStore(chunks).
		WithOccurredRangeChecker(checker)

	results, err := svc.Search(context.Background(), windowedQuery())
	if err != nil {
		t.Fatalf("Search returned error: %v — verification failure is non-fatal", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %+v, want empty: an unverifiable chunk candidate must not enter a windowed result", results)
	}
}

// TestSearch_Windowed_NoChecker_KeepsTheOldSkip pins the fail-closed default
// for any caller that has not wired a checker.
func TestSearch_Windowed_NoChecker_KeepsTheOldSkip(t *testing.T) {
	t.Parallel()

	chunkDoc := uuid.New()
	docs := &recordingDocSearcher{}
	chunks := &mockChunkSearcher{ftsResults: []store.ChunkSearchResult{
		chunkResult(chunkDoc, model.SourceSMS, 0.9),
	}}

	svc := NewService(docs, disabledEmbedder{}).WithChunkStore(chunks)
	results, err := svc.Search(context.Background(), windowedQuery())
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %+v, want empty when no verifier is configured", results)
	}
}

// TestSearch_NoWindow_ChunkLanesRunWithoutVerification is the control the task
// asks for explicitly: the verification path must not leak into unwindowed
// queries — the overwhelming majority — either as a behaviour change or as an
// extra database round-trip.
func TestSearch_NoWindow_ChunkLanesRunWithoutVerification(t *testing.T) {
	t.Parallel()

	chunkDoc := uuid.New()
	docs := &recordingDocSearcher{}
	chunks := &mockChunkSearcher{ftsResults: []store.ChunkSearchResult{
		chunkResult(chunkDoc, model.SourceSMS, 0.9),
	}}
	// The checker knows nothing about this document: if it were consulted, the
	// document would be filtered out and the test would fail.
	checker := &fakeOccurredChecker{occurredAt: map[uuid.UUID]time.Time{}}

	svc := NewService(docs, disabledEmbedder{}).
		WithChunkStore(chunks).
		WithOccurredRangeChecker(checker)

	results, err := svc.Search(context.Background(), model.SearchQuery{Query: "budget", Limit: 20})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != 1 || results[0].ID != chunkDoc {
		t.Fatalf("results = %+v, want the chunk hit %s — an unwindowed query must keep both chunk lanes", results, chunkDoc)
	}
	if checker.calls != 0 {
		t.Errorf("occurred_at verification ran %d times for a query with no window", checker.calls)
	}
}

// TestSearch_Windowed_OneSidedWindow_VerifiesChunkLanes: a half-bounded window
// is still a window, on both the constraint side and the recall side.
func TestSearch_Windowed_OneSidedWindow_VerifiesChunkLanes(t *testing.T) {
	t.Parallel()

	// Each case needs its own violating timestamp: a document from last week is
	// outside a "from" window but perfectly inside a "to" window.
	for _, tc := range []struct {
		name    string
		query   model.SearchQuery
		outside time.Time
	}{
		{
			name:    "from only",
			query:   model.SearchQuery{Query: "q", Limit: 20, OccurredFrom: &svcWindowFrom},
			outside: svcWindowFrom.Add(-72 * time.Hour),
		},
		{
			name:    "to only",
			query:   model.SearchQuery{Query: "q", Limit: 20, OccurredTo: &svcWindowTo},
			outside: svcWindowTo.Add(72 * time.Hour),
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inWindow, outOfWindow := uuid.New(), uuid.New()
			docs := &recordingDocSearcher{}
			chunks := &mockChunkSearcher{ftsResults: []store.ChunkSearchResult{
				chunkResult(outOfWindow, model.SourceSMS, 0.9),
				chunkResult(inWindow, model.SourceSMS, 0.8),
			}}
			checker := &fakeOccurredChecker{occurredAt: map[uuid.UUID]time.Time{
				inWindow:    inWindowTime(),
				outOfWindow: tc.outside,
			}}
			svc := NewService(docs, disabledEmbedder{}).
				WithChunkStore(chunks).
				WithOccurredRangeChecker(checker)

			results, err := svc.Search(context.Background(), tc.query)
			if err != nil {
				t.Fatalf("Search returned error: %v", err)
			}
			if len(results) != 1 || results[0].ID != inWindow {
				t.Fatalf("results = %+v, want only the in-window chunk %s", results, inWindow)
			}
		})
	}
}

// TestSearch_Windowed_ChunkVerification_UsesTheRequestedBounds guards against
// the verification drifting from the window the store was given: both must
// constrain the same interval, or the two halves of the result set answer
// different questions.
func TestSearch_Windowed_ChunkVerification_UsesTheRequestedBounds(t *testing.T) {
	t.Parallel()

	var gotFrom, gotTo *time.Time
	checker := &boundsRecordingChecker{onCall: func(from, to *time.Time) {
		gotFrom, gotTo = from, to
	}}

	docs := &recordingDocSearcher{}
	chunks := &mockChunkSearcher{ftsResults: []store.ChunkSearchResult{
		chunkResult(uuid.New(), model.SourceSMS, 0.9),
	}}
	svc := NewService(docs, disabledEmbedder{}).
		WithChunkStore(chunks).
		WithOccurredRangeChecker(checker)

	if _, err := svc.Search(context.Background(), windowedQuery()); err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if gotFrom == nil || !gotFrom.Equal(svcWindowFrom) {
		t.Errorf("verification lower bound = %v, want %s", gotFrom, svcWindowFrom)
	}
	if gotTo == nil || !gotTo.Equal(svcWindowTo) {
		t.Errorf("verification upper bound = %v, want %s", gotTo, svcWindowTo)
	}
}

type boundsRecordingChecker struct {
	onCall func(from, to *time.Time)
}

func (b *boundsRecordingChecker) FilterIDsByOccurredRange(_ context.Context, ids []uuid.UUID, from, to *time.Time) (map[uuid.UUID]struct{}, error) {
	b.onCall(from, to)
	out := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out, nil
}
