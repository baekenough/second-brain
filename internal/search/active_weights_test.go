package search

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// #214 — the promoted weights never reached the serving path.
//
// cmd/tune -promote writes an 'active' row into search_weights_history, and
// nothing read it. Every search kept using the compiled-in defaults, so the
// whole tuning loop — measure, gate, promote — ended in a table nobody
// consulted. Promotion looked like it worked and changed nothing.
//
// The tests below fix the four decisions this wiring has to make: when the row
// is read, what it loses to, what happens when the read fails, and that the
// feature is genuinely off by default.
// ---------------------------------------------------------------------------

// stubActiveWeights is an ActiveWeightsReader whose answer can be changed
// between requests, which is how the promote/rollback cases are expressed.
type stubActiveWeights struct {
	mu    sync.Mutex
	rec   *store.WeightsRecord
	err   error
	calls int
}

func (s *stubActiveWeights) Active(context.Context) (*store.WeightsRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.rec, s.err
}

func (s *stubActiveWeights) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *stubActiveWeights) set(rec *store.WeightsRecord, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec, s.err = rec, err
}

func activeRecord(w model.SearchWeights) *store.WeightsRecord {
	return &store.WeightsRecord{ID: 7, Status: store.WeightsStatusActive, Weights: w}
}

// promotedWeights is deliberately unlike the compiled defaults on every field
// so that "the active row was used" cannot be confused with "the defaults were
// used".
var promotedWeights = model.SearchWeights{
	FTSWeight:  0.30,
	VecWeight:  1.70,
	BigmWeight: 0.40,
	SummaryVec: 0.60,
	RRFK:       25,
}

func runSearchOnce(t *testing.T, svc *Service, docs *recordingDocSearcher, q model.SearchQuery) model.SearchWeights {
	t.Helper()
	if _, err := svc.Search(context.Background(), q); err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	return docs.gotQuery.Weights
}

// (a) An active row replaces the default weights the store is asked to use.
func TestSearch_ActiveWeights_PromotedRowBecomesTheDefault(t *testing.T) {
	t.Parallel()

	docs := &recordingDocSearcher{}
	reader := &stubActiveWeights{rec: activeRecord(promotedWeights)}
	svc := NewService(docs, disabledEmbedder{}).WithActiveWeights(reader, true)

	got := runSearchOnce(t, svc, docs, model.SearchQuery{Query: "예산"})
	if got != promotedWeights {
		t.Fatalf("store saw weights %+v, want the promoted row %+v — promotion must reach the serving path",
			got, promotedWeights)
	}
	if reader.callCount() == 0 {
		t.Fatal("the active row was never read")
	}
}

// (b) A per-request Weights beats the active row. internal/tune evaluates
// candidates by setting this field, so an active row that overrode it would
// make every candidate measure as the incumbent — the tuner would then compare
// a configuration against itself and promote on noise.
func TestSearch_ActiveWeights_PerRequestWeightsWin(t *testing.T) {
	t.Parallel()

	requested := model.SearchWeights{FTSWeight: 2.5, VecWeight: 0.1, BigmWeight: 3.0, RRFK: 99}

	docs := &recordingDocSearcher{}
	reader := &stubActiveWeights{rec: activeRecord(promotedWeights)}
	svc := NewService(docs, disabledEmbedder{}).WithActiveWeights(reader, true)

	got := runSearchOnce(t, svc, docs, model.SearchQuery{Query: "예산", Weights: requested})
	if got != requested {
		t.Fatalf("store saw weights %+v, want the caller's %+v — the active row is a DEFAULT, not an override",
			got, requested)
	}
	if reader.callCount() != 0 {
		t.Errorf("active row was read %d times for a request that specified its own weights; the lookup is pointless work",
			reader.callCount())
	}
}

// (c) No active row, and a failed lookup, both fall back to the compiled
// defaults — and the search still returns results. A tuning table is not a
// dependency search may die on.
func TestSearch_ActiveWeights_FallsBackToCompiledDefaults(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		reader *stubActiveWeights
	}{
		{"no active row", &stubActiveWeights{rec: nil, err: nil}},
		{"lookup fails", &stubActiveWeights{rec: nil, err: errors.New("connection refused")}},
		{"row present but weights are the zero value", &stubActiveWeights{rec: activeRecord(model.SearchWeights{})}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			hit := makeSearchResult(uuid.New(), "결과", 0.9)
			docs := &recordingDocSearcher{results: []*model.SearchResult{hit}}
			svc := NewService(docs, disabledEmbedder{}).WithActiveWeights(tc.reader, true)

			results, err := svc.Search(context.Background(), model.SearchQuery{Query: "예산"})
			if err != nil {
				t.Fatalf("Search returned error: %v — a weights lookup problem must never fail the search", err)
			}
			if len(results) != 1 {
				t.Fatalf("results = %d, want 1 — the search must still serve", len(results))
			}
			if got := docs.gotQuery.Weights; got != (model.SearchWeights{}) {
				t.Fatalf("store saw weights %+v, want the zero value (= compiled defaults)", got)
			}
		})
	}
}

// (d) With the flag off the table is not consulted at all. "Off" has to mean no
// query, not a query whose result is discarded: the row on a given deployment
// may be a configuration that was never validated there, and the cost of the
// lookup is the smaller half of the reason.
func TestSearch_ActiveWeights_FlagOff_NeverReadsTheTable(t *testing.T) {
	t.Parallel()

	docs := &recordingDocSearcher{}
	reader := &stubActiveWeights{rec: activeRecord(promotedWeights)}
	svc := NewService(docs, disabledEmbedder{}).WithActiveWeights(reader, false)

	got := runSearchOnce(t, svc, docs, model.SearchQuery{Query: "예산"})
	if reader.callCount() != 0 {
		t.Errorf("active row was read %d times with the flag off; the flag must gate the query itself",
			reader.callCount())
	}
	if got != (model.SearchWeights{}) {
		t.Errorf("store saw weights %+v with the flag off, want the zero value (= compiled defaults)", got)
	}
}

// (e) The row is re-read on every request, so a promotion or a rollback made by
// the separate cmd/tune process takes effect on the NEXT request with no
// restart and no invalidation call — which is the property the rollback
// procedure depends on. A cached value could only offer "within the TTL",
// because nothing in this process observes the other process's write.
func TestSearch_ActiveWeights_PromoteAndRollbackTakeEffectOnTheNextRequest(t *testing.T) {
	t.Parallel()

	docs := &recordingDocSearcher{}
	reader := &stubActiveWeights{} // nothing promoted yet
	svc := NewService(docs, disabledEmbedder{}).WithActiveWeights(reader, true)

	q := model.SearchQuery{Query: "예산"}

	if got := runSearchOnce(t, svc, docs, q); got != (model.SearchWeights{}) {
		t.Fatalf("before promotion the store saw %+v, want the compiled defaults", got)
	}

	// cmd/tune -promote, in another process.
	reader.set(activeRecord(promotedWeights), nil)
	if got := runSearchOnce(t, svc, docs, q); got != promotedWeights {
		t.Fatalf("after promotion the store saw %+v, want %+v on the very next request", got, promotedWeights)
	}

	// cmd/tune -rollback with nothing to restore: back to the defaults.
	reader.set(nil, nil)
	if got := runSearchOnce(t, svc, docs, q); got != (model.SearchWeights{}) {
		t.Fatalf("after rollback the store saw %+v, want the compiled defaults on the very next request", got)
	}

	if reader.callCount() != 3 {
		t.Fatalf("reader calls = %d, want 3 (one per request) — a cache here cannot honour 'next request'",
			reader.callCount())
	}
}
