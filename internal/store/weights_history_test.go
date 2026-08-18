package store

import (
	"context"
	"os"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// search_weights_history — real-database tests.
//
// The single-active invariant is enforced by a PARTIAL UNIQUE INDEX and the
// promotion sequence has to survive it mid-transaction; neither property can be
// observed without PostgreSQL. Skipped when TEST_DATABASE_URL is unset.
//
// TEST_DATABASE_URL must NEVER point at production. These tests clear
// search_weights_history before and after each case: the single-active
// invariant is global to the table, so a leftover row from another case would
// decide the outcome of the next one. That is safe here because the table is
// written only by the offline weight tuner and never by the serving path — and
// because a throwaway database is the only legitimate value for that variable.
// ---------------------------------------------------------------------------

func weightsTestStore(t *testing.T) *WeightsHistoryStore {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database weights history test")
	}
	pg, err := NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pg.Close)

	clear := func() {
		if _, err := pg.pool.Exec(context.Background(), `DELETE FROM search_weights_history`); err != nil {
			t.Fatalf("clear search_weights_history: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)
	return NewWeightsHistoryStore(pg)
}

func dummyWeights(fts float64) model.SearchWeights {
	return model.SearchWeights{
		FTSWeight: fts, VecWeight: 1, BigmWeight: 1,
		SummaryVec: 0.8, EntityWeight: 0.5, RRFK: 60,
	}
}

func insertProposed(t *testing.T, s *WeightsHistoryStore, fts float64) int64 {
	t.Helper()
	id, err := s.Insert(context.Background(), WeightsRecord{
		Weights:        dummyWeights(fts),
		Status:         WeightsStatusProposed,
		HoldoutQueries: 42,
		Metadata:       map[string]any{"rerank": false},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return id
}

func countActive(t *testing.T, s *WeightsHistoryStore) int {
	t.Helper()
	var n int
	if err := s.pg.pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM search_weights_history WHERE status = 'active'`).Scan(&n); err != nil {
		t.Fatalf("count active: %v", err)
	}
	return n
}

func TestActive_ReturnsNilWhenEmpty(t *testing.T) {
	s := weightsTestStore(t)
	got, err := s.Active(context.Background())
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if got != nil {
		t.Fatalf("Active = %+v, want nil when nothing has been promoted", got)
	}
}

// TestPromote_LeavesExactlyOneActive is the core invariant: whatever the
// sequence of promotions, the serving path must find one answer to "which
// weights are live?", never two.
func TestPromote_LeavesExactlyOneActive(t *testing.T) {
	s := weightsTestStore(t)
	ctx := context.Background()

	var last int64
	for i := 0; i < 3; i++ {
		id := insertProposed(t, s, 0.25*float64(i+1))
		if err := s.Promote(ctx, id); err != nil {
			t.Fatalf("promote %d: %v", i, err)
		}
		last = id
		if got := countActive(t, s); got != 1 {
			t.Fatalf("after promotion %d there are %d active rows, want 1", i, got)
		}
	}

	active, err := s.Active(ctx)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if active == nil || active.ID != last {
		t.Fatalf("Active = %+v, want the row promoted last (id %d)", active, last)
	}
	if active.PromotedAt == nil {
		t.Error("promoted row has no promoted_at timestamp")
	}
	if active.Weights != dummyWeights(0.75) {
		t.Errorf("Active weights = %+v, want %+v (JSON round-trip lost data)", active.Weights, dummyWeights(0.75))
	}
}

// TestPromote_IsAtomicUnderDuplicateActive proves the invariant lives in the
// database. An application-level check would pass here and still corrupt the
// table under two concurrent tuner runs.
func TestPromote_IsAtomicUnderDuplicateActive(t *testing.T) {
	s := weightsTestStore(t)
	ctx := context.Background()

	a := insertProposed(t, s, 0.5)
	b := insertProposed(t, s, 1.0)
	if err := s.Promote(ctx, a); err != nil {
		t.Fatalf("promote a: %v", err)
	}
	// Bypass the store deliberately: the point is that raw SQL cannot create a
	// second active row either.
	if _, err := s.pg.pool.Exec(ctx,
		`UPDATE search_weights_history SET status = 'active' WHERE id = $1`, b); err == nil {
		t.Fatal("database accepted a second active row; the single-active invariant is not enforced")
	}
	if got := countActive(t, s); got != 1 {
		t.Fatalf("active rows = %d, want 1", got)
	}
}

func TestPromote_UnknownIDFails(t *testing.T) {
	s := weightsTestStore(t)
	if err := s.Promote(context.Background(), 987654321); err == nil {
		t.Fatal("Promote accepted an unknown id")
	}
}

// TestRollback_RestoresPreviousActive pins the escape hatch: a promotion that
// turns out to be wrong must be undoable without a deploy.
func TestRollback_RestoresPreviousActive(t *testing.T) {
	s := weightsTestStore(t)
	ctx := context.Background()

	a := insertProposed(t, s, 0.5)
	b := insertProposed(t, s, 1.75)
	if err := s.Promote(ctx, a); err != nil {
		t.Fatalf("promote a: %v", err)
	}
	if err := s.Promote(ctx, b); err != nil {
		t.Fatalf("promote b: %v", err)
	}

	restored, err := s.Rollback(ctx)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if restored == nil || restored.ID != a {
		t.Fatalf("Rollback restored %+v, want the row promoted before (id %d)", restored, a)
	}
	if got := countActive(t, s); got != 1 {
		t.Fatalf("active rows = %d, want 1", got)
	}

	var statusB string
	if err := s.pg.pool.QueryRow(ctx,
		`SELECT status FROM search_weights_history WHERE id = $1`, b).Scan(&statusB); err != nil {
		t.Fatalf("read b: %v", err)
	}
	if statusB != WeightsStatusRolledBack {
		t.Errorf("rolled-back row status = %q, want %q", statusB, WeightsStatusRolledBack)
	}
}

// TestRollback_NoPreviousReturnsNilNoError covers the first-ever promotion:
// undoing it returns the system to "no active row", which is the code default.
func TestRollback_NoPreviousReturnsNilNoError(t *testing.T) {
	s := weightsTestStore(t)
	ctx := context.Background()

	a := insertProposed(t, s, 0.5)
	if err := s.Promote(ctx, a); err != nil {
		t.Fatalf("promote: %v", err)
	}
	restored, err := s.Rollback(ctx)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if restored != nil {
		t.Fatalf("Rollback = %+v, want nil (nothing to restore)", restored)
	}
	if got := countActive(t, s); got != 0 {
		t.Fatalf("active rows = %d, want 0 (fall back to the code defaults)", got)
	}
}

func TestRollback_WithNoActiveIsANoOp(t *testing.T) {
	s := weightsTestStore(t)
	restored, err := s.Rollback(context.Background())
	if err != nil {
		t.Fatalf("Rollback with nothing active: %v", err)
	}
	if restored != nil {
		t.Fatalf("Rollback = %+v, want nil", restored)
	}
}

// TestInsert_RejectsUnknownStatus keeps a typo from creating a status the
// promotion logic does not understand.
func TestInsert_RejectsUnknownStatus(t *testing.T) {
	s := weightsTestStore(t)
	_, err := s.Insert(context.Background(), WeightsRecord{
		Weights: dummyWeights(1), Status: "activated",
	})
	if err == nil {
		t.Fatal("Insert accepted an unknown status")
	}
}
