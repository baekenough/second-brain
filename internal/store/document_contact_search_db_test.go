package store

import (
	"context"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

func TestDB_SearchCallContactMetadata(t *testing.T) {
	pg := srcTestDB(t)
	ctx := context.Background()
	match := seedSrcDoc(t, pg, model.SourceCall, nil)
	deleted := seedSrcDoc(t, pg, model.SourceCall, nil)
	unrelated := seedSrcDoc(t, pg, model.SourceGmail, nil)
	for _, id := range []interface{}{match, deleted, unrelated} {
		if _, err := pg.pool.Exec(ctx, `UPDATE documents SET metadata = '{"contact_name":"테스트상대방"}', embedding = NULL WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pg.pool.Exec(ctx, `UPDATE documents SET status='deleted' WHERE id=$1`, deleted); err != nil {
		t.Fatal(err)
	}
	for _, hybrid := range []bool{false, true} {
		q := model.SearchQuery{Query: "테스트상대방", Limit: 50}
		if hybrid {
			q.Embedding = testEmbedding()
		}
		got, err := NewDocumentStore(pg).Search(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != match {
			t.Fatalf("hybrid=%v: metadata search returned %+v, want only %v", hybrid, got, match)
		}
		if got[0].Title != "zzdummy title" || got[0].Content != "zzdummy sentinel body" {
			t.Fatal("contact search modified document text")
		}
	}
}
