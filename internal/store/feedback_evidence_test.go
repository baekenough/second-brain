package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/baekenough/second-brain/internal/dataset"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// UpsertEvidence — real-database tests.
//
// This package has no SQL stub harness and a stub could not prove any of the
// properties that matter here: the ON CONFLICT target has to match a PARTIAL
// unique index expression exactly, and the split value is computed by the
// database. Everything below therefore runs against a real PostgreSQL instance
// addressed by TEST_DATABASE_URL and is skipped when that variable is unset.
//
// TEST_DATABASE_URL must NEVER point at production. Every row written here uses
// the sentinel session prefix below and is removed in t.Cleanup. No test data
// resembles a real query, name or document.
//
// The file lives in package store_test rather than store because it imports
// internal/dataset, which imports internal/store; an in-package test file would
// be an import cycle.
// ---------------------------------------------------------------------------

const evidenceTestSessionPrefix = "zz-dummy-evidence-"

func evidenceTestDB(t *testing.T) *store.Postgres {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database evidence feedback test")
	}
	pg, err := store.NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg
}

// seedEvidenceDoc inserts one throwaway document (feedback.document_id is an FK)
// and registers cleanup for every row this test writes.
func seedEvidenceDoc(t *testing.T, pg *store.Postgres) string {
	t.Helper()
	ctx := context.Background()
	docID := uuid.New()
	_, err := pg.Pool().Exec(ctx, `
		INSERT INTO documents (id, source_type, source_id, title, content, collected_at)
		VALUES ($1, 'filesystem', $2, 'dummy title', 'dummy content', now())`,
		docID, "zz-dummy-evidence-"+docID.String())
	if err != nil {
		t.Fatalf("seed document: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := pg.Pool().Exec(ctx,
			`DELETE FROM feedback WHERE session_id LIKE $1`, evidenceTestSessionPrefix+"%"); err != nil {
			t.Errorf("cleanup feedback: %v", err)
		}
		if _, err := pg.Pool().Exec(ctx, `DELETE FROM feedback WHERE document_id = $1`, docID); err != nil {
			t.Errorf("cleanup feedback by doc: %v", err)
		}
		if _, err := pg.Pool().Exec(ctx, `DELETE FROM documents WHERE id = $1`, docID); err != nil {
			t.Errorf("cleanup document: %v", err)
		}
	})
	return docID.String()
}

func evidenceSession(t *testing.T) string {
	t.Helper()
	return evidenceTestSessionPrefix + uuid.NewString()
}

func countEvidenceRows(t *testing.T, pg *store.Postgres, session string) int {
	t.Helper()
	var n int
	if err := pg.Pool().QueryRow(context.Background(),
		`SELECT count(*)::int FROM feedback WHERE session_id = $1`, session).Scan(&n); err != nil {
		t.Fatalf("count evidence rows: %v", err)
	}
	return n
}

// TestUpsertEvidence_SecondDocInSameSessionIsNotDeleted is the regression guard
// for the reason this function exists at all. FeedbackStore.Upsert deletes every
// row for a (user, session, source) tuple before inserting; reusing it for
// per-evidence votes would erase the labels already collected in the same
// conversation. Both documents must survive.
func TestUpsertEvidence_SecondDocInSameSessionIsNotDeleted(t *testing.T) {
	pg := evidenceTestDB(t)
	docA := seedEvidenceDoc(t, pg)
	docB := seedEvidenceDoc(t, pg)
	fs := store.NewFeedbackStore(pg)
	session := evidenceSession(t)
	ctx := context.Background()

	if _, _, err := fs.UpsertEvidence(ctx, store.EvidenceVote{
		SessionID: session, Query: "dummy evidence query alpha", DocumentID: docA, Thumbs: 1,
	}); err != nil {
		t.Fatalf("upsert docA: %v", err)
	}
	if _, _, err := fs.UpsertEvidence(ctx, store.EvidenceVote{
		SessionID: session, Query: "dummy evidence query alpha", DocumentID: docB, Thumbs: -1,
	}); err != nil {
		t.Fatalf("upsert docB: %v", err)
	}

	if got := countEvidenceRows(t, pg, session); got != 2 {
		t.Fatalf("rows for session = %d, want 2 (a prior label was destroyed)", got)
	}
}

func TestUpsertEvidence_RepeatSameVoteTogglesToZero(t *testing.T) {
	pg := evidenceTestDB(t)
	doc := seedEvidenceDoc(t, pg)
	fs := store.NewFeedbackStore(pg)
	session := evidenceSession(t)
	ctx := context.Background()
	vote := store.EvidenceVote{SessionID: session, Query: "dummy evidence query beta", DocumentID: doc, Thumbs: 1}

	id1, thumbs1, err := fs.UpsertEvidence(ctx, vote)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if thumbs1 != 1 {
		t.Fatalf("first upsert thumbs = %d, want 1", thumbs1)
	}
	id2, thumbs2, err := fs.UpsertEvidence(ctx, vote)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if id1 != id2 {
		t.Errorf("second upsert created a new row (%d != %d); the call is not idempotent", id1, id2)
	}
	if thumbs2 != 0 {
		t.Errorf("second identical vote thumbs = %d, want 0 (toggle off)", thumbs2)
	}
	if got := countEvidenceRows(t, pg, session); got != 1 {
		t.Errorf("rows for session = %d, want 1", got)
	}
}

func TestUpsertEvidence_OppositeVoteOverwrites(t *testing.T) {
	pg := evidenceTestDB(t)
	doc := seedEvidenceDoc(t, pg)
	fs := store.NewFeedbackStore(pg)
	session := evidenceSession(t)
	ctx := context.Background()

	if _, _, err := fs.UpsertEvidence(ctx, store.EvidenceVote{
		SessionID: session, Query: "dummy evidence query gamma", DocumentID: doc, Thumbs: 1,
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	_, thumbs, err := fs.UpsertEvidence(ctx, store.EvidenceVote{
		SessionID: session, Query: "dummy evidence query gamma", DocumentID: doc, Thumbs: -1,
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if thumbs != -1 {
		t.Errorf("opposite vote thumbs = %d, want -1", thumbs)
	}
	if got := countEvidenceRows(t, pg, session); got != 1 {
		t.Errorf("rows for session = %d, want 1", got)
	}
}

// TestUpsertEvidence_DoesNotTouchLegacyRows proves the partial index and the
// upsert stay inside source='ask_evidence'. Rows collected by the existing
// generic endpoint must be untouched.
func TestUpsertEvidence_DoesNotTouchLegacyRows(t *testing.T) {
	pg := evidenceTestDB(t)
	doc := seedEvidenceDoc(t, pg)
	fs := store.NewFeedbackStore(pg)
	session := evidenceSession(t)
	ctx := context.Background()
	query := "dummy evidence query delta"

	// Two legacy rows that would collide under a global unique index.
	for i := 0; i < 2; i++ {
		if _, err := fs.Record(ctx, store.Feedback{
			Query: &query, DocumentID: &doc, Source: "search",
			SessionID: &session, Thumbs: 1,
		}); err != nil {
			t.Fatalf("seed legacy row %d: %v", i, err)
		}
	}

	if _, _, err := fs.UpsertEvidence(ctx, store.EvidenceVote{
		SessionID: session, Query: query, DocumentID: doc, Thumbs: 1,
	}); err != nil {
		t.Fatalf("upsert evidence: %v", err)
	}

	var legacy int
	if err := pg.Pool().QueryRow(ctx,
		`SELECT count(*)::int FROM feedback WHERE session_id = $1 AND source = 'search'`,
		session).Scan(&legacy); err != nil {
		t.Fatalf("count legacy: %v", err)
	}
	if legacy != 2 {
		t.Fatalf("legacy rows = %d, want 2 (evidence upsert modified unrelated rows)", legacy)
	}
}

// TestUpsertEvidence_SplitMatchesSQLBackfill is the only defence against the Go
// and SQL hash rules drifting apart. If they drift, a query can be assigned to
// train at insert time and to holdout by the backfill (or the reverse), and the
// holdout set stops being independent of what the optimiser saw.
func TestUpsertEvidence_SplitMatchesSQLBackfill(t *testing.T) {
	pg := evidenceTestDB(t)
	doc := seedEvidenceDoc(t, pg)
	fs := store.NewFeedbackStore(pg)
	ctx := context.Background()

	sawTrain, sawHoldout := false, false
	for i := 0; i < 20; i++ {
		session := evidenceSession(t)
		query := fmt.Sprintf("dummy split probe %d", i)
		id, _, err := fs.UpsertEvidence(ctx, store.EvidenceVote{
			SessionID: session, Query: query, DocumentID: doc, Thumbs: 1,
		})
		if err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}

		var stored, backfill string
		if err := pg.Pool().QueryRow(ctx, `
			SELECT split,
			       CASE WHEN abs(('x' || substr(md5('sbfeedback:v1:' || lower(btrim(query))),1,8))::bit(32)::bigint) % 100 < 70
			            THEN 'train' ELSE 'holdout' END
			FROM feedback WHERE id = $1`, id).Scan(&stored, &backfill); err != nil {
			t.Fatalf("read split %d: %v", i, err)
		}

		goSplit := dataset.SplitOf(query)
		if stored != backfill {
			t.Errorf("query %d: stored split %q != backfill expression %q", i, stored, backfill)
		}
		if stored != goSplit {
			t.Errorf("query %d: stored split %q != dataset.SplitOf %q", i, stored, goSplit)
		}
		switch goSplit {
		case dataset.SplitTrain:
			sawTrain = true
		case dataset.SplitHoldout:
			sawHoldout = true
		}
	}
	if !sawTrain || !sawHoldout {
		t.Fatalf("probe set did not exercise both splits (train=%v holdout=%v)", sawTrain, sawHoldout)
	}
}

// --- EvalPairsBySplit -------------------------------------------------------

func TestEvalPairsBySplit_RejectsUnknownSplit(t *testing.T) {
	pg := evidenceTestDB(t)
	es := store.NewEvalStore(pg)
	for _, split := range []string{"", "all", "TRAIN"} {
		if _, err := es.EvalPairsBySplit(context.Background(), split); err == nil {
			t.Errorf("EvalPairsBySplit(%q) returned no error; an unrecognised split must not "+
				"silently widen to the full labelled set", split)
		}
	}
}

// TestEvalPairsBySplit_ReturnsOnlyRequestedSplit exercises the real SQL: the
// split filter is a bound parameter on three separate queries, and a typing or
// placeholder mistake there is invisible to a stub.
func TestEvalPairsBySplit_ReturnsOnlyRequestedSplit(t *testing.T) {
	pg := evidenceTestDB(t)
	doc := seedEvidenceDoc(t, pg)
	fs := store.NewFeedbackStore(pg)
	es := store.NewEvalStore(pg)
	ctx := context.Background()

	// Seed one query on each side of the split so both branches are covered.
	seeded := map[string]string{} // query -> split
	for i := 0; i < 12; i++ {
		query := fmt.Sprintf("dummy split lane probe %d", i)
		if _, _, err := fs.UpsertEvidence(ctx, store.EvidenceVote{
			SessionID: evidenceSession(t), Query: query, DocumentID: doc, Thumbs: 1,
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		seeded[query] = dataset.SplitOf(query)
	}

	for _, split := range []string{dataset.SplitTrain, dataset.SplitHoldout} {
		pairs, err := es.EvalPairsBySplit(ctx, split)
		if err != nil {
			t.Fatalf("EvalPairsBySplit(%q): %v", split, err)
		}
		for _, p := range pairs {
			if want, ok := seeded[p.Query]; ok && want != split {
				t.Errorf("query hash %s expected in %q but returned for %q",
					dataset.QueryHash(p.Query), want, split)
			}
		}
	}
}

// TestBuildFromFeedback_UnfilteredStillWorks guards the refactor that gave
// BuildFromFeedback a shared implementation with EvalPairsBySplit: the three
// queries now carry a bound parameter they did not have before, and a mistake
// there would break the existing /api/v1/eval/export path.
func TestBuildFromFeedback_UnfilteredStillWorks(t *testing.T) {
	pg := evidenceTestDB(t)
	doc := seedEvidenceDoc(t, pg)
	fs := store.NewFeedbackStore(pg)
	es := store.NewEvalStore(pg)
	ctx := context.Background()

	query := "dummy unfiltered probe"
	if _, _, err := fs.UpsertEvidence(ctx, store.EvidenceVote{
		SessionID: evidenceSession(t), Query: query, DocumentID: doc, Thumbs: 1,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pairs, err := es.BuildFromFeedback(ctx)
	if err != nil {
		t.Fatalf("BuildFromFeedback: %v", err)
	}
	for _, p := range pairs {
		if p.Query == query {
			return
		}
	}
	t.Fatalf("BuildFromFeedback did not return the seeded query (got %d pairs)", len(pairs))
}
