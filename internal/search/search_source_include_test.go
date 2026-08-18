package search

import (
	"context"
	"fmt"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// #196 — the include filter never reached the chunk lanes.
//
// The document store applies SourceType/SourceTypes in SQL. The chunk lanes
// cannot: ChunkSearcher takes only a query/vector and a limit. The service's
// post-filter looked at ExcludeSourceTypes only, so a caller asking for ONE
// source got that source from the store plus whatever the chunk lanes happened
// to match — from any source.
//
// Measured in production: source_type=calendar with limit=20 returned 14
// calendar documents from the store and 6 documents of other source types that
// the chunk vector lane merged into the free slots. The user asked for one
// source and got four.
//
// The window gate partially hides this: a query that also sets a date range
// skips the chunk lanes entirely, so "내일 일정" looks correct. A query that
// names a source WITHOUT a window ("홍길동과의 통화 내용") does not. A query
// planner produces the second shape in bulk, which is why this is a
// prerequisite rather than a cleanup.
// ---------------------------------------------------------------------------

// chunkResult builds a chunk-lane row for the given document/source.
func chunkResult(id uuid.UUID, src model.SourceType, score float64) store.ChunkSearchResult {
	return store.ChunkSearchResult{
		Chunk:          store.Chunk{ID: 1, DocumentID: id, Content: "chunk text"},
		Score:          score,
		Rank:           score,
		DocumentSource: string(src),
		DocumentStatus: "active",
	}
}

// TestSearch_IncludeFilter_AppliesToChunkVectorLane reproduces the measured
// #196 scenario: 14 in-source documents from the store, 6 other-source
// documents from the chunk vector lane, limit 20.
func TestSearch_IncludeFilter_AppliesToChunkVectorLane(t *testing.T) {
	t.Parallel()

	const (
		storeHits  = 14
		chunkHits  = 6
		limit      = 20
		wantSource = model.SourceCalendar
	)

	storeResults := make([]*model.SearchResult, 0, storeHits)
	for i := 0; i < storeHits; i++ {
		r := makeSearchResult(uuid.New(), fmt.Sprintf("calendar entry %d", i), 1.0-float64(i)/100)
		r.SourceType = wantSource
		storeResults = append(storeResults, r)
	}

	// The chunk lane matched six documents from three other sources — exactly
	// the slots the store's narrower result left open.
	otherSources := []model.SourceType{
		model.SourceSMS, model.SourceGmail, model.SourceCallTranscript,
		model.SourceSMS, model.SourceGmail, model.SourceSMS,
	}
	chunkRows := make([]store.ChunkSearchResult, 0, chunkHits)
	for i, src := range otherSources {
		chunkRows = append(chunkRows, chunkResult(uuid.New(), src, 0.99-float64(i)/100))
	}

	docs := &recordingDocSearcher{results: storeResults}
	chunks := &mockChunkSearcher{vectorResults: chunkRows}
	svc := NewService(docs, stubEnabledEmbedder{}).WithChunkStore(chunks)

	src := wantSource
	results, err := svc.Search(context.Background(), model.SearchQuery{
		Query:      "일정",
		Limit:      limit,
		SourceType: &src,
	})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	var leaked int
	for _, r := range results {
		if r.SourceType != wantSource {
			leaked++
			t.Errorf("result %s has source %q, but the caller asked for %q only", r.ID, r.SourceType, wantSource)
		}
	}
	if leaked != 0 {
		t.Fatalf("%d/%d results came from a source the caller excluded", leaked, len(results))
	}
	if len(results) != storeHits {
		t.Errorf("got %d results, want the %d in-source documents", len(results), storeHits)
	}
}

// TestSearch_IncludeFilter_AppliesToChunkFTSFallback covers the other chunk
// lane. The fallback fires when the primary path found nothing, which is
// precisely when an unfiltered lane does the most damage: every returned
// document comes from it.
func TestSearch_IncludeFilter_AppliesToChunkFTSFallback(t *testing.T) {
	t.Parallel()

	wanted := uuid.New()
	docs := &recordingDocSearcher{} // store found nothing
	chunks := &mockChunkSearcher{ftsResults: []store.ChunkSearchResult{
		chunkResult(uuid.New(), model.SourceSMS, 0.9),
		chunkResult(wanted, model.SourceCalendar, 0.8),
		chunkResult(uuid.New(), model.SourceGmail, 0.7),
	}}

	src := model.SourceCalendar
	svc := NewService(docs, disabledEmbedder{}).WithChunkStore(chunks)
	results, err := svc.Search(context.Background(), model.SearchQuery{
		Query:      "일정",
		Limit:      20,
		SourceType: &src,
	})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	if len(results) != 1 || results[0].ID != wanted {
		t.Fatalf("results = %+v, want only the calendar chunk hit %s", results, wanted)
	}
}

// TestSearch_MultiSourceInclude_AppliesToChunkLanes is the plural form: a
// planner emitting ["gmail","sms"] must get gmail and sms and nothing else.
func TestSearch_MultiSourceInclude_AppliesToChunkLanes(t *testing.T) {
	t.Parallel()

	gmailDoc, smsDoc, slackDoc := uuid.New(), uuid.New(), uuid.New()

	docs := &recordingDocSearcher{}
	chunks := &mockChunkSearcher{ftsResults: []store.ChunkSearchResult{
		chunkResult(gmailDoc, model.SourceGmail, 0.9),
		chunkResult(slackDoc, model.SourceSlack, 0.85),
		chunkResult(smsDoc, model.SourceSMS, 0.8),
	}}

	svc := NewService(docs, disabledEmbedder{}).WithChunkStore(chunks)
	results, err := svc.Search(context.Background(), model.SearchQuery{
		Query:       "예약",
		Limit:       20,
		SourceTypes: []model.SourceType{model.SourceGmail, model.SourceSMS},
	})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	got := map[uuid.UUID]bool{}
	for _, r := range results {
		got[r.ID] = true
	}
	if !got[gmailDoc] || !got[smsDoc] {
		t.Errorf("results = %+v, want both included sources present", results)
	}
	if got[slackDoc] {
		t.Errorf("results contain the slack document %s, which is outside the include set", slackDoc)
	}
}

// TestSearch_IncludeFilter_PassedToStoreUnmodified is the wiring assertion:
// the service post-filters the chunk lanes, but the store is still the
// component that must apply the filter in SQL, so it has to see both fields.
func TestSearch_IncludeFilter_PassedToStoreUnmodified(t *testing.T) {
	t.Parallel()

	docs := &recordingDocSearcher{}
	svc := NewService(docs, disabledEmbedder{})

	if _, err := svc.Search(context.Background(), model.SearchQuery{
		Query:       "q",
		Limit:       20,
		SourceTypes: []model.SourceType{model.SourceGmail, model.SourceSMS},
	}); err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	got := docs.gotQuery.IncludeSourceTypes()
	if len(got) != 2 {
		t.Fatalf("store received include set %v, want both requested sources", got)
	}
}

// ---------------------------------------------------------------------------
// include / exclude conflict.
//
// Rule: EXCLUDE WINS. A source type named by both filters is dropped.
//
// The rule has to be stated somewhere, because the two filters have different
// origins: excludes are policy (the insight echo-chamber guard is injected by
// the service and by /ask, not requested by the user), includes are a request.
// Letting a request override a policy exclusion would let a query planner
// re-open the guard by naming the excluded source. The cost is that a query
// naming ONLY excluded sources returns nothing — which is why the conflict is
// resolved and logged, and why /ask reconciles the two BEFORE calling search
// rather than letting an empty result happen silently (see
// internal/api/ask_retrieval.go).
// ---------------------------------------------------------------------------

func TestSearch_IncludeExcludeConflict_ExcludeWins(t *testing.T) {
	t.Parallel()

	gmailDoc, smsDoc := uuid.New(), uuid.New()

	docs := &recordingDocSearcher{}
	chunks := &mockChunkSearcher{ftsResults: []store.ChunkSearchResult{
		chunkResult(gmailDoc, model.SourceGmail, 0.9),
		chunkResult(smsDoc, model.SourceSMS, 0.8),
	}}

	svc := NewService(docs, disabledEmbedder{}).WithChunkStore(chunks)
	results, err := svc.Search(context.Background(), model.SearchQuery{
		Query:              "q",
		Limit:              20,
		SourceTypes:        []model.SourceType{model.SourceGmail, model.SourceSMS},
		ExcludeSourceTypes: []model.SourceType{model.SourceGmail},
	})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	for _, r := range results {
		if r.ID == gmailDoc {
			t.Errorf("gmail document %s survived although gmail is in ExcludeSourceTypes", gmailDoc)
		}
	}
	var sawSMS bool
	for _, r := range results {
		if r.ID == smsDoc {
			sawSMS = true
		}
	}
	if !sawSMS {
		t.Errorf("results = %+v, want the non-conflicting sms document to survive", results)
	}
}

// TestSearch_InsightInIncludeSet_OptsOutOfDefaultExclusion keeps the existing
// opt-in contract working through the plural field: the default insight
// exclusion is added only when the caller did not ask for insight documents.
func TestSearch_InsightInIncludeSet_OptsOutOfDefaultExclusion(t *testing.T) {
	t.Parallel()

	docs := &recordingDocSearcher{}
	svc := NewService(docs, disabledEmbedder{})

	if _, err := svc.Search(context.Background(), model.SearchQuery{
		Query:       "q",
		Limit:       20,
		SourceTypes: []model.SourceType{model.SourceInsight},
	}); err != nil {
		t.Fatalf("Search returned error: %v", err)
	}

	for _, st := range docs.gotQuery.ExcludeSourceTypes {
		if st == model.SourceInsight {
			t.Fatalf("insight was excluded although the caller asked for it: %+v", docs.gotQuery)
		}
	}
}
