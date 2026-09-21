package store

import (
	"context"
	"os"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Real-database checks for Upsert/UpsertTracked's classification-protected
// metadata merge (see document.go's upsertMetadataMergeSQL doc comment).
//
// A stub/mock store cannot exercise this: the bug this file pins down is a
// PostgreSQL jsonb expression (`EXCLUDED.metadata || COALESCE((SELECT
// jsonb_object_agg(...) FROM jsonb_each(documents.metadata) ...), '{}')`),
// and this project has already shipped SQL that compiled, passed stub
// tests, and failed at runtime (RETURNING referencing EXCLUDED). Skipped
// unless TEST_DATABASE_URL is set — same convention as this package's other
// *_db_test.go files (see golden_db_test.go's doc comment). TEST_DATABASE_URL
// must NEVER point at production: run a throwaway pgvector+pg_bigm
// container with the full migration set applied (main_test.go's TestMain).
// ---------------------------------------------------------------------------

const upsertMetaTestPrefix = "zz-dummy-upsertmeta-"

func upsertMetaTestDB(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database upsert-metadata test")
	}
	pg, err := NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pg.Close)
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(context.Background(),
			`DELETE FROM documents WHERE source_id LIKE $1`, upsertMetaTestPrefix+"%")
	})
	return pg
}

// TestDB_Upsert_PreservesClassificationMetadataAcrossRecollection pins the
// bug fix directly: a document a human golden-set judgment tagged
// classifier="user"/retention="keep" must keep that tag after a collector
// re-upserts the SAME (source_type, source_id) with a fresh collector-owned
// metadata snapshot — before this fix, `metadata = EXCLUDED.metadata`
// silently wiped the classification tag because EXCLUDED.metadata (the
// collector's payload) never carries it in the first place, letting the
// document's next re-classification override a human "relevant" judgment.
func TestDB_Upsert_PreservesClassificationMetadataAcrossRecollection(t *testing.T) {
	pg := upsertMetaTestDB(t)
	store := NewDocumentStore(pg)
	ctx := context.Background()

	sourceID := upsertMetaTestPrefix + uuid.New().String()
	doc := &model.Document{
		SourceType: model.SourceSMS,
		SourceID:   sourceID,
		Title:      "zzdummy title",
		Content:    "zzdummy sentinel body v1",
		Metadata:   map[string]any{"sender": "010-0000-0000"},
	}
	if err := store.Upsert(ctx, doc); err != nil {
		t.Fatalf("initial Upsert: %v", err)
	}

	// A human golden-set judgment tags the document (internal/store/golden.go's
	// UpsertJudgments writes exactly this shape).
	if _, err := pg.pool.Exec(ctx, `
		UPDATE documents
		SET metadata = metadata || jsonb_build_object('retention', 'keep', 'classifier', 'user', 'classified_at', now())
		WHERE id = $1
	`, doc.ID); err != nil {
		t.Fatalf("seed classification metadata: %v", err)
	}

	// Collector re-syncs the SAME (source_type, source_id) with an entirely
	// new collector-owned metadata payload — no retention/classifier keys at
	// all, which is what every real collector actually sends.
	recollected := &model.Document{
		SourceType: model.SourceSMS,
		SourceID:   sourceID,
		Title:      "zzdummy title v2",
		Content:    "zzdummy sentinel body v2",
		Metadata:   map[string]any{"sender": "010-1111-1111", "direction": "inbound"},
	}
	if err := store.Upsert(ctx, recollected); err != nil {
		t.Fatalf("re-collection Upsert: %v", err)
	}

	var meta map[string]any
	if err := pg.pool.QueryRow(ctx, `SELECT metadata FROM documents WHERE id = $1`, doc.ID).Scan(&meta); err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if meta["retention"] != model.RetentionKeep {
		t.Errorf("retention after re-collection = %v, want %q (must survive a collector re-upsert)", meta["retention"], model.RetentionKeep)
	}
	if meta["classifier"] != "user" {
		t.Errorf(`classifier after re-collection = %v, want "user"`, meta["classifier"])
	}
	if meta["sender"] != "010-1111-1111" {
		t.Errorf("sender after re-collection = %v, want the NEW collector value (collector-owned keys must still be replaced wholesale)", meta["sender"])
	}
	if meta["direction"] != "inbound" {
		t.Errorf(`direction after re-collection = %v, want "inbound" (a brand-new collector key must still be added)`, meta["direction"])
	}
}

// TestDB_UpsertTracked_PreservesClassificationMetadataAcrossRecollection
// mirrors the Upsert case above for UpsertTracked — the variant the ingest
// pipeline actually calls — and additionally checks that content-change
// detection (contentChanged) is unaffected by the metadata merge change.
func TestDB_UpsertTracked_PreservesClassificationMetadataAcrossRecollection(t *testing.T) {
	pg := upsertMetaTestDB(t)
	store := NewDocumentStore(pg)
	ctx := context.Background()

	sourceID := upsertMetaTestPrefix + uuid.New().String()
	doc := &model.Document{
		SourceType: model.SourceGmail,
		SourceID:   sourceID,
		Title:      "zzdummy mail v1",
		Content:    "zzdummy sentinel mail body v1",
		Metadata:   map[string]any{"from": "zzdummy-a@example.com"},
	}
	if _, err := store.UpsertTracked(ctx, doc); err != nil {
		t.Fatalf("initial UpsertTracked: %v", err)
	}

	if _, err := pg.pool.Exec(ctx, `
		UPDATE documents
		SET metadata = metadata || jsonb_build_object('retention', 'disposable', 'classifier', 'user', 'classified_at', now())
		WHERE id = $1
	`, doc.ID); err != nil {
		t.Fatalf("seed classification metadata: %v", err)
	}

	recollected := &model.Document{
		SourceType: model.SourceGmail,
		SourceID:   sourceID,
		Title:      "zzdummy mail v2",
		Content:    "zzdummy sentinel mail body v2",
		Metadata:   map[string]any{"from": "zzdummy-b@example.com"},
	}
	changed, err := store.UpsertTracked(ctx, recollected)
	if err != nil {
		t.Fatalf("re-collection UpsertTracked: %v", err)
	}
	if !changed {
		t.Errorf("contentChanged = false, want true (content actually changed on re-collection)")
	}

	var meta map[string]any
	if err := pg.pool.QueryRow(ctx, `SELECT metadata FROM documents WHERE id = $1`, doc.ID).Scan(&meta); err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	// A human "noise" judgment (retention=disposable) must survive just as
	// much as a "relevant" one — the merge protects the KEY, not a specific
	// value.
	if meta["retention"] != model.RetentionDisposable {
		t.Errorf("retention after re-collection = %v, want %q (a human judgment must survive regardless of its value)", meta["retention"], model.RetentionDisposable)
	}
	if meta["from"] != "zzdummy-b@example.com" {
		t.Errorf("from after re-collection = %v, want the NEW collector value", meta["from"])
	}
}
