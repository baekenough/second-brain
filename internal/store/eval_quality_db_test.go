package store

import (
	"context"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
	"os"
	"testing"
)

func TestDB_EvalSignedManualVotesAndEligibility(t *testing.T) {
	pg := srcTestDB(t)
	ctx := context.Background()
	s := NewEvalStore(pg)
	query := "zz-dummy-eval-" + uuid.NewString()
	positive := seedSrcDoc(t, pg, model.SourceGmail, nil)
	negative := seedSrcDoc(t, pg, model.SourceGmail, nil)
	disposable := seedSrcDoc(t, pg, model.SourceGmail, nil)
	t.Cleanup(func() { _, _ = pg.pool.Exec(ctx, `DELETE FROM feedback WHERE query=$1`, query) })
	_, err := pg.pool.Exec(ctx, `INSERT INTO feedback(query,document_id,source,thumbs) VALUES($1,$2,'manual',1),($1,$3,'manual',-1),($1,$3,'manual',1),($1,$4,'manual',1)`, query, positive, negative, disposable)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pg.pool.Exec(ctx, `UPDATE documents SET metadata='{"retention":"disposable"}' WHERE id=$1`, disposable); err != nil {
		t.Fatal(err)
	}
	pairs, err := s.BuildFromFeedback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got EvalPair
	for _, p := range pairs {
		if p.Query == query {
			got = p
		}
	}
	if len(got.RelevantDocIDs) != 2 || len(got.IrrelevantDocIDs) != 1 || got.IrrelevantDocIDs[0] != negative.String() {
		t.Fatalf("signed votes lost: %+v", got)
	}
	filtered, excluded, err := s.FilterSearchEligible(ctx, []EvalPair{got})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || len(filtered[0].RelevantDocIDs) != 1 || filtered[0].RelevantDocIDs[0] != positive.String() || excluded.Relevant != 1 {
		t.Fatalf("eligibility: %+v exclusions %+v", filtered, excluded)
	}
	// The original labels survive evaluation filtering unchanged.
	var count int
	if err = pg.pool.QueryRow(ctx, `SELECT count(*) FROM feedback WHERE query=$1`, query).Scan(&count); err != nil || count != 4 {
		t.Fatalf("labels changed: %d %v", count, err)
	}
}

func TestDB_GoldenExportKeepsNegativeOnlyQueries(t *testing.T) {
	pg := goldenTestDB(t)
	ctx := context.Background()
	q := seedGoldenQuery(t, pg, goldenTestSentinel+"negative-only", "manual", "open")
	doc := seedGoldenDocument(t, pg, nil)
	_, err := pg.pool.Exec(ctx, `INSERT INTO golden_judgments(query_id,document_id,judgment,judge) VALUES($1,$2,'irrelevant','user')`, q, doc)
	if err != nil {
		t.Fatal(err)
	}
	pairs, err := NewGoldenStore(pg).ExportEvalPairs(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || len(pairs[0].RelevantDocIDs) != 0 || len(pairs[0].IrrelevantDocIDs) != 1 || pairs[0].IrrelevantDocIDs[0] != doc.String() {
		t.Fatalf("negative-only query omitted: %+v", pairs)
	}
}

func TestDB_EvalBaselineRequiresMatchingCompleteProvenance(t *testing.T) {
	pg := srcTestDB(t)
	ctx := context.Background()
	s := NewEvalMetricsStore(pg)
	token := uuid.NewString()
	t.Cleanup(func() { _, _ = pg.pool.Exec(ctx, `DELETE FROM eval_metrics WHERE code_revision=$1`, token) })
	cases := []EvalMetricsRecord{
		{NDCG10: 0.5, ConfigHash: token, LabelHash: token, Attempted: 2, Pairs: 2},
		{NDCG10: 0.9, ConfigHash: "different", LabelHash: token, Attempted: 2},
		{NDCG10: 0.8, ConfigHash: token, LabelHash: "different", Attempted: 2},
		{NDCG10: 1, ConfigHash: token, LabelHash: token, Attempted: 2, Failed: 1},
		{NDCG10: 1}, // legacy data has no matching profile
	}
	for _, rec := range cases {
		rec.CodeRevision = token
		if err := s.Save(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LatestMatching(ctx, token, token)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.NDCG10 != 0.5 || got.Attempted != 2 {
		t.Fatalf("wrong baseline: %+v", got)
	}
	if got, err = s.LatestMatching(ctx, "", token); err != nil || got != nil {
		t.Fatalf("legacy matched: %+v %v", got, err)
	}
}

func TestDB_ReadOnlyConnectorRejectsWrites(t *testing.T) {
	_ = srcTestDB(t)
	pg, err := NewReadOnlyPostgres(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	var readonly string
	if err = pg.pool.QueryRow(context.Background(), `SHOW default_transaction_read_only`).Scan(&readonly); err != nil || readonly != "on" {
		t.Fatalf("read only setting: %q %v", readonly, err)
	}
	if _, err = pg.pool.Exec(context.Background(), `DELETE FROM eval_metrics WHERE false`); err == nil {
		t.Fatal("read-only connection accepted write")
	}
}
