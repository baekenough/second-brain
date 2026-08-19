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
// Sort="recent" survives RRF fusion.
//
// RRF decides WHICH documents come back; Sort decides IN WHICH ORDER they are
// shown. mergeRRF used to do both: it re-sorted the whole merged set by fused
// score, so the store's ORDER BY — the only place the caller's Sort is honoured
// — was discarded the moment a single chunk-vector hit survived its lane.
//
// That was invisible while "recent" meant one clause (DESC), because a merged
// result set that lost its recency order still looked plausible. It stopped
// being invisible once the direction became window-dependent (occurred_at ASC
// for a window that lies entirely in the future): a forward-looking /ask query
// runs the chunk lanes whenever an OccurredRangeChecker is wired — production
// wires one — so "다음 주 일정" would be ordered soonest-first only when the
// chunk lane happened to contribute nothing.
//
// The tests below fix the contract in both directions, pin the relevance path
// against regression, and pin determinism: mergeRRF flattens a map, so without
// a total order the output order is whatever the runtime's map iteration gave.
// ---------------------------------------------------------------------------

// recentResult builds a store-shaped result: unlike a chunk-lane row it carries
// the event time the recency order is defined on.
func recentResult(id uuid.UUID, title string, occurred time.Time, score float64) *model.SearchResult {
	o := occurred
	return &model.SearchResult{
		Document: model.Document{
			ID:          id,
			Title:       title,
			OccurredAt:  &o,
			CollectedAt: occurred.Add(time.Minute), // ingested slightly after the event
		},
		Score:     score,
		MatchType: "hybrid",
	}
}

func idsOf(results []*model.SearchResult) []uuid.UUID {
	out := make([]uuid.UUID, len(results))
	for i, r := range results {
		out[i] = r.ID
	}
	return out
}

func sameIDs(got []*model.SearchResult, want ...uuid.UUID) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i].ID != want[i] {
			return false
		}
	}
	return true
}

// recencySetup wires the one configuration that reproduces the bug: a store
// result already in the requested order, plus a chunk-vector hit on the LAST of
// those documents. The fusion boost lifts that document to the top of the fused
// score ranking, which is precisely what must no longer decide the output order.
type recencySetup struct {
	svc       *Service
	boosted   uuid.UUID // in the store result AND in the chunk lane (score boost)
	plain     uuid.UUID // in the store result only
	chunkOnly uuid.UUID // reachable only through the chunk lane: no event time
}

func newRecencySetup(t *testing.T, first, second *model.SearchResult, from, to *time.Time, occurred map[uuid.UUID]time.Time) recencySetup {
	t.Helper()

	chunkOnly := uuid.New()
	docs := &recordingDocSearcher{results: []*model.SearchResult{first, second}}
	chunks := &mockChunkSearcher{vectorResults: []store.ChunkSearchResult{
		chunkResult(second.ID, model.SourceCalendar, 0.99),
		chunkResult(chunkOnly, model.SourceCalendar, 0.98),
	}}

	all := map[uuid.UUID]time.Time{}
	for k, v := range occurred {
		all[k] = v
	}
	checker := &fakeOccurredChecker{occurredAt: all}

	svc := NewService(docs, stubEnabledEmbedder{}).
		WithChunkStore(chunks).
		WithOccurredRangeChecker(checker)

	return recencySetup{svc: svc, boosted: second.ID, plain: first.ID, chunkOnly: chunkOnly}
}

// (a) Future window: the merged set must be ordered soonest-first, matching the
// store's `occurred_at ASC` clause for a window that lies entirely ahead.
func TestSearch_SortRecent_FutureWindow_ChunkFusion_OrdersSoonestFirst(t *testing.T) {
	t.Parallel()

	now := time.Now()
	from, to := now.Add(24*time.Hour), now.Add(30*24*time.Hour)

	soonest := recentResult(uuid.New(), "내일 회의", now.Add(25*time.Hour), 0.9)
	later := recentResult(uuid.New(), "다음 주 회의", now.Add(72*time.Hour), 0.5)

	s := newRecencySetup(t, soonest, later, &from, &to, map[uuid.UUID]time.Time{
		soonest.ID: now.Add(25 * time.Hour),
		later.ID:   now.Add(72 * time.Hour),
	})
	// The chunk-only document must survive the window verification, otherwise
	// the "no event time" branch is never exercised.
	s.svc.occurredChecker.(*fakeOccurredChecker).occurredAt[s.chunkOnly] = now.Add(48 * time.Hour)

	results, err := s.svc.Search(context.Background(), model.SearchQuery{
		Query:        "다음 주 일정",
		Limit:        20,
		Sort:         "recent",
		OccurredFrom: &from,
		OccurredTo:   &to,
	})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	// soonest, then later, then the chunk-only hit which carries no event time
	// and therefore cannot be placed among documents that do.
	if !sameIDs(results, soonest.ID, later.ID, s.chunkOnly) {
		t.Fatalf("order = %v, want soonest-first [%s %s %s]; the chunk-vector boost on %q must not reorder a future window",
			idsOf(results), soonest.ID, later.ID, s.chunkOnly, later.Title)
	}
}

// (b) Past window: newest-first, matching COALESCE(occurred_at, collected_at)
// DESC. The pre-existing direction must not move.
func TestSearch_SortRecent_PastWindow_ChunkFusion_OrdersNewestFirst(t *testing.T) {
	t.Parallel()

	now := time.Now()
	from, to := now.Add(-30*24*time.Hour), now.Add(-time.Hour)

	newest := recentResult(uuid.New(), "어제 통화", now.Add(-2*time.Hour), 0.9)
	older := recentResult(uuid.New(), "지난주 통화", now.Add(-48*time.Hour), 0.5)

	s := newRecencySetup(t, newest, older, &from, &to, map[uuid.UUID]time.Time{
		newest.ID: now.Add(-2 * time.Hour),
		older.ID:  now.Add(-48 * time.Hour),
	})
	s.svc.occurredChecker.(*fakeOccurredChecker).occurredAt[s.chunkOnly] = now.Add(-24 * time.Hour)

	results, err := s.svc.Search(context.Background(), model.SearchQuery{
		Query:        "지난주 통화",
		Limit:        20,
		Sort:         "recent",
		OccurredFrom: &from,
		OccurredTo:   &to,
	})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if !sameIDs(results, newest.ID, older.ID, s.chunkOnly) {
		t.Fatalf("order = %v, want newest-first [%s %s %s]", idsOf(results), newest.ID, older.ID, s.chunkOnly)
	}
}

// (c) Relevance (and the default empty Sort) must keep the fused-score order
// exactly as it is today: the boosted document first, then the remaining store
// hit, then the chunk-only hit. This is the regression guard for the ~all of
// traffic that does not ask for a recency order.
func TestSearch_SortRelevance_ChunkFusion_KeepsRRFScoreOrder(t *testing.T) {
	t.Parallel()

	for _, sort := range []string{"relevance", ""} {
		sort := sort
		t.Run("sort="+sort, func(t *testing.T) {
			t.Parallel()

			now := time.Now()
			from, to := now.Add(-30*24*time.Hour), now.Add(-time.Hour)

			first := recentResult(uuid.New(), "첫 문서", now.Add(-2*time.Hour), 0.9)
			second := recentResult(uuid.New(), "둘째 문서", now.Add(-48*time.Hour), 0.5)

			s := newRecencySetup(t, first, second, &from, &to, map[uuid.UUID]time.Time{
				first.ID:  now.Add(-2 * time.Hour),
				second.ID: now.Add(-48 * time.Hour),
			})
			s.svc.occurredChecker.(*fakeOccurredChecker).occurredAt[s.chunkOnly] = now.Add(-24 * time.Hour)

			results, err := s.svc.Search(context.Background(), model.SearchQuery{
				Query:        "통화",
				Limit:        20,
				Sort:         sort,
				OccurredFrom: &from,
				OccurredTo:   &to,
			})
			if err != nil {
				t.Fatalf("Search returned error: %v", err)
			}

			// RRF: second = 1/62 (primary rank 2) + 1/61 (secondary rank 1),
			// first = 1/61, chunkOnly = 1/62.
			if !sameIDs(results, second.ID, first.ID, s.chunkOnly) {
				t.Fatalf("order = %v, want fused-score order [%s %s %s] — relevance ranking must be untouched",
					idsOf(results), second.ID, first.ID, s.chunkOnly)
			}
		})
	}
}

// (d) No chunk fusion: the store is the sole producer of the result set and its
// ORDER BY is already the requested order, so the service must hand it back
// verbatim. The store result here is deliberately NOT in recency order — if the
// service re-sorted unconditionally it would override the store rather than
// restore it, and the two would then disagree in ways nothing can observe.
func TestSearch_SortRecent_WithoutChunkFusion_PreservesStoreOrder(t *testing.T) {
	t.Parallel()

	now := time.Now()
	older := recentResult(uuid.New(), "오래된 문서", now.Add(-48*time.Hour), 0.9)
	newer := recentResult(uuid.New(), "최근 문서", now.Add(-2*time.Hour), 0.5)

	cases := []struct {
		name string
		svc  func() *Service
	}{
		{
			name: "no chunk store configured",
			svc: func() *Service {
				docs := &recordingDocSearcher{results: []*model.SearchResult{older, newer}}
				return NewService(docs, disabledEmbedder{})
			},
		},
		{
			name: "chunk lane returns nothing",
			svc: func() *Service {
				docs := &recordingDocSearcher{results: []*model.SearchResult{older, newer}}
				return NewService(docs, stubEnabledEmbedder{}).
					WithChunkStore(&mockChunkSearcher{})
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			results, err := tc.svc().Search(context.Background(), model.SearchQuery{
				Query: "문서",
				Limit: 20,
				Sort:  "recent",
			})
			if err != nil {
				t.Fatalf("Search returned error: %v", err)
			}
			if !sameIDs(results, older.ID, newer.ID) {
				t.Fatalf("order = %v, want the store's own order [%s %s]", idsOf(results), older.ID, newer.ID)
			}
		})
	}
}

// (e) Two documents at the same instant must come back in the same order every
// time. mergeRRF flattens a map, so "equal keys keep their input order" is not
// a guarantee here — the input order is whatever map iteration produced. The
// tie is broken on document id, which is stable across processes as well as
// across runs.
func TestSearch_SortRecent_EqualTimestamps_AreDeterministic(t *testing.T) {
	t.Parallel()

	now := time.Now()
	from, to := now.Add(-30*24*time.Hour), now.Add(-time.Hour)
	sameInstant := now.Add(-5 * time.Hour)

	a := recentResult(uuid.New(), "동시 문서 A", sameInstant, 0.7)
	b := recentResult(uuid.New(), "동시 문서 B", sameInstant, 0.7)

	// Reversed ranks between the two lanes make the fused scores identical
	// (1/61 + 1/62 either way), so neither time nor score can break the tie.
	docs := &recordingDocSearcher{results: []*model.SearchResult{a, b}}
	chunks := &mockChunkSearcher{vectorResults: []store.ChunkSearchResult{
		chunkResult(b.ID, model.SourceSMS, 0.99),
		chunkResult(a.ID, model.SourceSMS, 0.98),
	}}
	checker := &fakeOccurredChecker{occurredAt: map[uuid.UUID]time.Time{
		a.ID: sameInstant,
		b.ID: sameInstant,
	}}
	svc := NewService(docs, stubEnabledEmbedder{}).
		WithChunkStore(chunks).
		WithOccurredRangeChecker(checker)

	q := model.SearchQuery{
		Query:        "동시 문서",
		Limit:        20,
		Sort:         "recent",
		OccurredFrom: &from,
		OccurredTo:   &to,
	}

	first, err := svc.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("results = %v, want both documents", idsOf(first))
	}
	// 40 repetitions: Go randomises map iteration per range statement, so a
	// non-total order shows up as a flake within a handful of runs.
	for i := 0; i < 40; i++ {
		again, err := svc.Search(context.Background(), q)
		if err != nil {
			t.Fatalf("Search returned error on run %d: %v", i, err)
		}
		if !sameIDs(again, first[0].ID, first[1].ID) {
			t.Fatalf("run %d order = %v, first run = %v — equal timestamps must not depend on map iteration",
				i, idsOf(again), idsOf(first))
		}
	}
	// Pin the tie-break itself, not merely its stability: a run-to-run stable
	// but process-to-process arbitrary order would still be a reproducibility
	// hazard for eval runs.
	wantFirst, wantSecond := a.ID, b.ID
	if wantSecond.String() < wantFirst.String() {
		wantFirst, wantSecond = wantSecond, wantFirst
	}
	if !sameIDs(first, wantFirst, wantSecond) {
		t.Fatalf("tie order = %v, want id-ascending [%s %s]", idsOf(first), wantFirst, wantSecond)
	}
}

// (f) The service and the store must derive the direction from ONE rule. This
// half asserts the service side against model.SearchQuery.RecencyAscending;
// TestSortOrder_DirectionFollowsTheSharedRule (internal/store) asserts the SQL
// side against the same function. If either copy drifts, one of the two fails —
// which is the only way this divergence is observable at all, since a merged
// result set and a store-only result set look identical in the response.
func TestSearch_SortRecent_DirectionFollowsTheSharedRule(t *testing.T) {
	t.Parallel()

	now := time.Now()
	offset := func(d time.Duration) *time.Time { t := now.Add(d); return &t }

	// Each case carries two event times INSIDE its own window, so the chunk
	// candidate survives verification and the fusion actually fires — the whole
	// point being to compare the fused order against the shared rule.
	cases := []struct {
		name          string
		from, to      *time.Time
		earlier, late time.Duration
	}{
		{"future window", offset(24 * time.Hour), offset(48 * time.Hour), 25 * time.Hour, 47 * time.Hour},
		{"future window, open upper bound", offset(time.Hour), nil, 2 * time.Hour, 72 * time.Hour},
		{"past window", offset(-48 * time.Hour), offset(-24 * time.Hour), -47 * time.Hour, -25 * time.Hour},
		{"straddles now", offset(-24 * time.Hour), offset(24 * time.Hour), -23 * time.Hour, 23 * time.Hour},
		{"upper bound only", nil, offset(24 * time.Hour), -72 * time.Hour, 23 * time.Hour},
		{"no window at all", nil, nil, -72 * time.Hour, -2 * time.Hour},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			earlier := recentResult(uuid.New(), "이른 문서", now.Add(tc.earlier), 0.9)
			late := recentResult(uuid.New(), "늦은 문서", now.Add(tc.late), 0.5)

			docs := &recordingDocSearcher{results: []*model.SearchResult{earlier, late}}
			chunks := &mockChunkSearcher{vectorResults: []store.ChunkSearchResult{
				chunkResult(late.ID, model.SourceCalendar, 0.99),
			}}
			checker := &fakeOccurredChecker{occurredAt: map[uuid.UUID]time.Time{
				earlier.ID: now.Add(tc.earlier),
				late.ID:    now.Add(tc.late),
			}}
			svc := NewService(docs, stubEnabledEmbedder{}).
				WithChunkStore(chunks).
				WithOccurredRangeChecker(checker)

			results, err := svc.Search(context.Background(), model.SearchQuery{
				Query:        "일정",
				Limit:        20,
				Sort:         "recent",
				OccurredFrom: tc.from,
				OccurredTo:   tc.to,
			})
			if err != nil {
				t.Fatalf("Search returned error: %v", err)
			}
			if !sameIDs(results, earlier.ID, late.ID) && !sameIDs(results, late.ID, earlier.ID) {
				t.Fatalf("results = %v, want both documents — the fusion must have fired for this assertion to mean anything",
					idsOf(results))
			}

			// Guard the guard: the assertion below is only meaningful while the
			// chunk lane really merged. mergeRRF overwrites Score with the fused
			// value and the boost lands on `late`, so a higher score there is
			// proof both that fusion ran and that the pre-fix code would have
			// put `late` first — which is the wrong answer on every ascending
			// case.
			scoreOf := func(id uuid.UUID) float64 {
				for _, r := range results {
					if r.ID == id {
						return r.Score
					}
				}
				return 0
			}
			if scoreOf(late.ID) <= scoreOf(earlier.ID) {
				t.Fatalf("fused scores late=%v earlier=%v: the chunk lane did not boost %s, so this case asserts nothing",
					scoreOf(late.ID), scoreOf(earlier.ID), late.ID)
			}

			// The expectation is READ OFF the shared rule rather than restated,
			// so moving the rule moves this test with it and a second copy of
			// the rule inside the service fails here.
			ascending := model.SearchQuery{OccurredFrom: tc.from}.RecencyAscending(now)
			wantFirst := late.ID
			if ascending {
				wantFirst = earlier.ID
			}
			if results[0].ID != wantFirst {
				t.Fatalf("ascending=%v: order = %v, want %s first — the service must use the same direction rule as the store",
					ascending, idsOf(results), wantFirst)
			}
		})
	}
}
