package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/baekenough/second-brain/internal/dataset"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/tune"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Fixtures. Everything here is synthetic: invented question strings and made-up
// document identifiers. No production data, no personal text.
// ---------------------------------------------------------------------------

const (
	goodDoc = "doc-relevant"
	badDoc  = "doc-known-bad"
)

// fakeSearcher ranks the relevant document first once the FTS weight crosses a
// threshold, which gives coordinate search something real to find and the
// holdout gate a genuine improvement to judge.
type fakeSearcher struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeSearcher) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	f.mu.Lock()
	f.calls++
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	order := []string{badDoc, goodDoc}
	if q.Weights.FTSWeight >= 1.5 {
		order = []string{goodDoc, badDoc}
	}
	out := make([]*model.SearchResult, 0, len(order))
	for _, id := range order {
		out = append(out, &model.SearchResult{Document: model.Document{ID: fixtureUUIDs[id]}})
	}
	return out, nil
}

// fixtureUUIDs maps the two fixture names to stable UUIDs so that
// SearchResult.ID.String() is comparable with the eval pairs' document IDs.
// Invented identifiers — they correspond to no real document.
var fixtureUUIDs = map[string]uuid.UUID{
	goodDoc: uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001"),
	badDoc:  uuid.MustParse("bbbbbbbb-0000-4000-8000-000000000002"),
}

func pairsFor(prefix string, n int) []store.EvalPair {
	out := make([]store.EvalPair, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, store.EvalPair{
			ID:               int64(i + 1),
			Query:            fmt.Sprintf("%s synthetic question %d", prefix, i),
			RelevantDocIDs:   []string{fixtureUUIDs[goodDoc].String()},
			IrrelevantDocIDs: []string{fixtureUUIDs[badDoc].String()},
		})
	}
	return out
}

type stubSource struct {
	train, holdout int
}

func (s *stubSource) EvalPairsBySplit(_ context.Context, split string) ([]store.EvalPair, error) {
	switch split {
	case dataset.SplitTrain:
		return pairsFor("train", s.train), nil
	case dataset.SplitHoldout:
		return pairsFor("holdout", s.holdout), nil
	default:
		return nil, fmt.Errorf("unexpected split %q", split)
	}
}

func loaderFor(train, holdout int) loaderFunc {
	src := &stubSource{train: train, holdout: holdout}
	return func(ctx context.Context) (*dataset.TrainSet, *dataset.HoldoutSet, error) {
		return dataset.Load(ctx, src)
	}
}

// recordingHistory mirrors the state machine that
// internal/store/weights_history.go implements in SQL. The real transitions are
// pinned against PostgreSQL in that package's tests; this fake exists so the
// command's decision logic can be tested without a database.
type recordingHistory struct {
	mu       sync.Mutex
	rows     map[int64]*store.WeightsRecord
	order    []int64
	nextID   int64
	inserts  []store.WeightsRecord
	promotes []int64
	rollback int
	err      error
}

func newHistory() *recordingHistory {
	return &recordingHistory{rows: map[int64]*store.WeightsRecord{}, nextID: 1}
}

func (h *recordingHistory) Insert(_ context.Context, rec store.WeightsRecord) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return 0, h.err
	}
	id := h.nextID
	h.nextID++
	rec.ID = id
	h.rows[id] = &rec
	h.order = append(h.order, id)
	h.inserts = append(h.inserts, rec)
	return id, nil
}

func (h *recordingHistory) Promote(_ context.Context, id int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec, ok := h.rows[id]
	if !ok {
		return store.ErrWeightsRecordNotFound
	}
	for _, r := range h.rows {
		if r.Status == store.WeightsStatusActive && r.ID != id {
			r.Status = store.WeightsStatusSuperseded
		}
	}
	rec.Status = store.WeightsStatusActive
	h.promotes = append(h.promotes, id)
	return nil
}

func (h *recordingHistory) Rollback(_ context.Context) (*store.WeightsRecord, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rollback++
	var active *store.WeightsRecord
	for _, r := range h.rows {
		if r.Status == store.WeightsStatusActive {
			active = r
		}
	}
	if active == nil {
		return nil, nil
	}
	active.Status = store.WeightsStatusRolledBack
	// Most recently superseded row wins, matching ORDER BY promoted_at DESC, id DESC.
	var previous *store.WeightsRecord
	for i := len(h.order) - 1; i >= 0; i-- {
		if r := h.rows[h.order[i]]; r.Status == store.WeightsStatusSuperseded {
			previous = r
			break
		}
	}
	if previous == nil {
		return nil, nil
	}
	previous.Status = store.WeightsStatusActive
	out := *previous
	return &out, nil
}

func (h *recordingHistory) Active(_ context.Context) (*store.WeightsRecord, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.rows {
		if r.Status == store.WeightsStatusActive {
			out := *r
			return &out, nil
		}
	}
	return nil, nil
}

func (h *recordingHistory) counts() (inserts, promotes, rollbacks int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.inserts), len(h.promotes), h.rollback
}

func testDeps(t *testing.T, train, holdout int) (deps, *recordingHistory, *fakeSearcher) {
	t.Helper()
	hist := newHistory()
	search := &fakeSearcher{}
	return deps{Load: loaderFor(train, holdout), Search: search, History: hist}, hist, search
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestRun_DoesNothingWhenFlagUnset is the deployment guarantee: the binary can
// be shipped and scheduled before anyone has decided to turn tuning on, and it
// will not read labels, will not search, and will not write history.
func TestRun_DoesNothingWhenFlagUnset(t *testing.T) {
	for _, v := range []string{"", "0", "false", "no", "off"} {
		t.Setenv("TUNE_ENABLED", v)
		d, hist, search := testDeps(t, 40, 40)
		loaded := false
		inner := d.Load
		d.Load = func(ctx context.Context) (*dataset.TrainSet, *dataset.HoldoutSet, error) {
			loaded = true
			return inner(ctx)
		}
		for _, mode := range []string{modePropose, modePromote, modeRollback} {
			_, err := run(context.Background(), d, options{Mode: mode})
			if !errors.Is(err, errDisabled) {
				t.Fatalf("TUNE_ENABLED=%q mode=%s: err = %v, want errDisabled", v, mode, err)
			}
		}
		if loaded {
			t.Errorf("TUNE_ENABLED=%q: labels were read with the feature off", v)
		}
		if i, p, r := hist.counts(); i+p+r != 0 {
			t.Errorf("TUNE_ENABLED=%q: history touched (%d inserts, %d promotes, %d rollbacks)", v, i, p, r)
		}
		if search.calls != 0 {
			t.Errorf("TUNE_ENABLED=%q: %d searches ran with the feature off", v, search.calls)
		}
	}
}

// TestRun_Under30HoldoutQueriesRefusesToPromote is the sample-size gate seen
// from the outside: a candidate that would otherwise be a landslide win is
// recorded as a proposal and nothing changes.
func TestRun_Under30HoldoutQueriesRefusesToPromote(t *testing.T) {
	t.Setenv("TUNE_ENABLED", "true")
	d, hist, _ := testDeps(t, 40, 29)

	out, err := run(context.Background(), d, options{Mode: modePromote})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Promoted {
		t.Fatal("promoted on 29 holdout queries")
	}
	if out.GateReason != tune.ReasonHoldoutTooSmall {
		t.Errorf("GateReason = %q, want %q", out.GateReason, tune.ReasonHoldoutTooSmall)
	}
	inserts, promotes, _ := hist.counts()
	if inserts != 1 {
		t.Fatalf("%d history rows written, want exactly 1 (the refused proposal must still be recorded)", inserts)
	}
	if promotes != 0 {
		t.Fatalf("%d promotions, want 0", promotes)
	}
	rec := hist.inserts[0]
	if rec.Status != store.WeightsStatusProposed {
		t.Errorf("recorded status = %q, want %q", rec.Status, store.WeightsStatusProposed)
	}
	if rec.GateReason == nil || !strings.Contains(*rec.GateReason, "29") {
		t.Errorf("gate reason %v does not say how many queries there were", rec.GateReason)
	}
}

// The mirror image: with a large enough sample and a real improvement, the
// promotion actually happens — otherwise the gate would be indistinguishable
// from a permanent "no".
func TestRun_PromotesOnClearWinWithEnoughQueries(t *testing.T) {
	t.Setenv("TUNE_ENABLED", "true")
	d, hist, _ := testDeps(t, 40, 40)

	out, err := run(context.Background(), d, options{Mode: modePromote})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Promoted {
		t.Fatalf("candidate not promoted; gate said %q", out.GateReason)
	}
	if out.Weights.FTSWeight < 1.5 {
		t.Errorf("promoted weights %v did not find the synthetic optimum", out.Weights)
	}
	_, promotes, _ := hist.counts()
	if promotes != 1 {
		t.Fatalf("%d promotions, want 1", promotes)
	}
	active, _ := hist.Active(context.Background())
	if active == nil || active.ID != out.HistoryID {
		t.Fatalf("active row = %v, want the promoted row %d", active, out.HistoryID)
	}
	if v, ok := active.Metadata["rerank"]; !ok || v != false {
		t.Errorf("metadata.rerank = %v, want false recorded so the numbers are not mistaken for live ones", v)
	}
}

// -propose measures and records but never changes the live configuration, even
// when the gate would have allowed it.
func TestRun_ProposeNeverPromotes(t *testing.T) {
	t.Setenv("TUNE_ENABLED", "true")
	d, hist, _ := testDeps(t, 40, 40)

	out, err := run(context.Background(), d, options{Mode: modePropose})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Promoted {
		t.Fatal("-propose promoted a candidate")
	}
	if !out.GatePassed {
		t.Fatal("expected the gate to pass so that this test is about the mode, not the gate")
	}
	inserts, promotes, _ := hist.counts()
	if inserts != 1 || promotes != 0 {
		t.Fatalf("inserts=%d promotes=%d, want 1 and 0", inserts, promotes)
	}
}

func TestRun_DryRunWritesNothing(t *testing.T) {
	t.Setenv("TUNE_ENABLED", "true")
	d, hist, _ := testDeps(t, 40, 40)

	out, err := run(context.Background(), d, options{Mode: modePromote, DryRun: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Promoted || out.HistoryID != 0 {
		t.Fatalf("dry run changed state: %+v", out)
	}
	if i, p, r := hist.counts(); i+p+r != 0 {
		t.Fatalf("dry run wrote history: %d inserts, %d promotes, %d rollbacks", i, p, r)
	}
}

// TestRun_StopsWhenHoldoutBudgetIsSpent proves the isolation budget is a stop,
// not a warning: handed a holdout set that has already been looked at twice,
// the run aborts instead of quietly promoting on a missing measurement.
func TestRun_StopsWhenHoldoutBudgetIsSpent(t *testing.T) {
	t.Setenv("TUNE_ENABLED", "true")
	d, hist, _ := testDeps(t, 40, 40)

	inner := d.Load
	d.Load = func(ctx context.Context) (*dataset.TrainSet, *dataset.HoldoutSet, error) {
		train, holdout, err := inner(ctx)
		if err != nil {
			return nil, nil, err
		}
		spend := func(context.Context, string, int) ([]string, error) { return []string{goodDoc}, nil }
		for i := 0; i < dataset.DefaultHoldoutBudget; i++ {
			if _, err := holdout.Evaluate(ctx, spend); err != nil {
				return nil, nil, err
			}
		}
		return train, holdout, nil
	}

	_, err := run(context.Background(), d, options{Mode: modePromote})
	if !errors.Is(err, dataset.ErrHoldoutBudgetExhausted) {
		t.Fatalf("err = %v, want dataset.ErrHoldoutBudgetExhausted", err)
	}
	if i, p, _ := hist.counts(); i+p != 0 {
		t.Fatalf("history written after an unjudgeable run: %d inserts, %d promotes", i, p)
	}
}

// TestRun_RollbackRestoresPreviousWeights is the "we can undo it" path: after
// two promotions, one rollback puts the earlier configuration back and reports
// which row it restored.
func TestRun_RollbackRestoresPreviousWeights(t *testing.T) {
	t.Setenv("TUNE_ENABLED", "true")
	d, hist, _ := testDeps(t, 40, 40)
	ctx := context.Background()

	first := model.SearchWeights{FTSWeight: 1.0, VecWeight: 1.0, BigmWeight: 1.0, SummaryVec: 0.3, EntityWeight: 0.3, RRFK: 60}
	second := model.SearchWeights{FTSWeight: 2.0, VecWeight: 0.5, BigmWeight: 0.25, SummaryVec: 0.8, EntityWeight: 0.8, RRFK: 20}
	idA, err := hist.Insert(ctx, store.WeightsRecord{Weights: first, Status: store.WeightsStatusProposed})
	if err != nil {
		t.Fatal(err)
	}
	idB, err := hist.Insert(ctx, store.WeightsRecord{Weights: second, Status: store.WeightsStatusProposed})
	if err != nil {
		t.Fatal(err)
	}
	if err := hist.Promote(ctx, idA); err != nil {
		t.Fatal(err)
	}
	if err := hist.Promote(ctx, idB); err != nil {
		t.Fatal(err)
	}

	out, err := run(ctx, d, options{Mode: modeRollback})
	if err != nil {
		t.Fatalf("run rollback: %v", err)
	}
	if out.Weights != first {
		t.Fatalf("restored weights = %+v, want %+v", out.Weights, first)
	}
	if out.HistoryID != idA {
		t.Errorf("restored history id = %d, want %d", out.HistoryID, idA)
	}
	active, _ := hist.Active(ctx)
	if active == nil || active.ID != idA {
		t.Fatalf("active row after rollback = %v, want %d", active, idA)
	}
	if hist.rows[idB].Status != store.WeightsStatusRolledBack {
		t.Errorf("row B status = %q, want %q", hist.rows[idB].Status, store.WeightsStatusRolledBack)
	}
}

// Rolling back with nothing promoted is not an error: the system is already at
// its compiled-in defaults, which is the state rollback aims for.
func TestRun_RollbackWithNothingToRestore(t *testing.T) {
	t.Setenv("TUNE_ENABLED", "true")
	d, _, _ := testDeps(t, 40, 40)

	out, err := run(context.Background(), d, options{Mode: modeRollback})
	if err != nil {
		t.Fatalf("run rollback: %v", err)
	}
	if out.HistoryID != 0 {
		t.Fatalf("HistoryID = %d, want 0 (no row restored)", out.HistoryID)
	}
}

func TestRun_RejectsUnknownMode(t *testing.T) {
	t.Setenv("TUNE_ENABLED", "true")
	d, _, _ := testDeps(t, 40, 40)
	if _, err := run(context.Background(), d, options{Mode: "sideways"}); err == nil {
		t.Fatal("run accepted an unknown mode")
	}
}

// The search path must never be asked to rerank or to run HyDE from the batch
// host: the reranker is unreachable from there and HyDE would make every sweep
// measure a different retrieval. RunnerFor enforces it; this checks the command
// actually goes through RunnerFor.
func TestRun_SearchesWithoutRerankOrHyDE(t *testing.T) {
	t.Setenv("TUNE_ENABLED", "true")
	hist := newHistory()
	probe := &recordingSearcher{}
	d := deps{Load: loaderFor(40, 40), Search: probe, History: hist}

	if _, err := run(context.Background(), d, options{Mode: modePropose}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if probe.sawRerank || probe.sawHyDE {
		t.Fatalf("batch search used rerank=%v hyde=%v", probe.sawRerank, probe.sawHyDE)
	}
	if probe.zeroWeights {
		t.Fatal("a search ran with zero weights, which search.Service reads as 'use the service defaults'")
	}
}

type recordingSearcher struct {
	mu          sync.Mutex
	sawRerank   bool
	sawHyDE     bool
	zeroWeights bool
}

func (r *recordingSearcher) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	r.mu.Lock()
	if q.UseRerank {
		r.sawRerank = true
	}
	if q.UseHyDE {
		r.sawHyDE = true
	}
	if (q.Weights == model.SearchWeights{}) {
		r.zeroWeights = true
	}
	r.mu.Unlock()
	return []*model.SearchResult{{Document: model.Document{ID: fixtureUUIDs[goodDoc]}}}, nil
}

// selectMode is the last line of defence against a typo that would change the
// live configuration in the opposite direction from what was intended.
func TestSelectMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		propose, promote, rollback bool
		want                       string
		wantErr                    bool
	}{
		{want: modePropose},
		{propose: true, want: modePropose},
		{promote: true, want: modePromote},
		{rollback: true, want: modeRollback},
		{promote: true, rollback: true, wantErr: true},
		{propose: true, promote: true, wantErr: true},
	}
	for i, c := range cases {
		got, err := selectMode(c.propose, c.promote, c.rollback)
		if c.wantErr {
			if err == nil {
				t.Errorf("case %d: expected an error for an ambiguous flag combination", i)
			}
			continue
		}
		if err != nil {
			t.Errorf("case %d: %v", i, err)
		}
		if got != c.want {
			t.Errorf("case %d: mode = %q, want %q", i, got, c.want)
		}
	}
}
