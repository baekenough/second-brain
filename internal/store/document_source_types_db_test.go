package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
)

// ---------------------------------------------------------------------------
// Real-database checks for the statements this package builds with fmt.Sprintf.
//
// The builder tests next door assert the SHAPE of the SQL. They cannot assert
// that PostgreSQL accepts it, or that pgx encodes a []model.SourceType into a
// parameter that `= ANY($N)` can compare against a text column — and this
// project has already shipped SQL that compiled, passed stub tests, and failed
// at runtime (EXCLUDED referenced inside RETURNING).
//
// Everything here is skipped unless TEST_DATABASE_URL is set, so `go test ./...`
// stays green without a database. TEST_DATABASE_URL must NEVER point at
// production: run a throwaway container instead. Rows are scoped by the
// sentinel source_id prefix below and removed in t.Cleanup.
// ---------------------------------------------------------------------------

// srcTestPrefix scopes both the seeded rows and the cleanup DELETE.
const srcTestPrefix = "zz-dummy-srctypes-"

func srcTestDB(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-database source-filter test")
	}
	pg, err := NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pg.Close)

	ensureSrcTestSchema(t, pg)
	t.Cleanup(func() {
		_, _ = pg.pool.Exec(context.Background(),
			`DELETE FROM documents WHERE source_id LIKE $1`, srcTestPrefix+"%")
	})
	return pg
}

// ensureSrcTestSchema creates the minimum needed to execute the search
// statements, and ONLY what is missing.
//
// It deliberately does not apply migrations/: 006 requires the pg_bigm
// extension, which the pgvector images do not ship, so a full migration run
// fails before it reaches anything relevant here. The hybrid statement calls
// bigm_similarity(), so a no-op stub stands in when the real extension is
// absent — the ranking expression is not what these tests assert, the WHERE
// predicates are. Both guards are conditional so that a database which DOES
// have the real schema/extension is left untouched.
func ensureSrcTestSchema(t *testing.T, pg *Postgres) {
	t.Helper()
	ctx := context.Background()

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS documents (
			id uuid PRIMARY KEY,
			source_type text NOT NULL,
			source_id text NOT NULL,
			title text NOT NULL DEFAULT '',
			content text NOT NULL DEFAULT '',
			metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
			embedding vector(3),
			status text NOT NULL DEFAULT 'active',
			deleted_at timestamptz,
			occurred_at timestamptz,
			collected_at timestamptz NOT NULL DEFAULT now(),
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now(),
			title_summary text,
			bullet_summary text,
			summary_embedding vector(3),
			tsv tsvector GENERATED ALWAYS AS (
				to_tsvector('simple', coalesce(title,'') || ' ' || coalesce(content,''))
			) STORED
		)`,
		`DO $do$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'bigm_similarity') THEN
				EXECUTE $f$CREATE FUNCTION bigm_similarity(text, text)
					RETURNS float8 LANGUAGE sql IMMUTABLE AS 'SELECT 0.0::float8'$f$;
			END IF;
		END
		$do$;`,
	}
	for _, s := range stmts {
		if _, err := pg.pool.Exec(ctx, s); err != nil {
			t.Fatalf("prepare test schema: %v", err)
		}
	}
}

// seedSrcDoc inserts one throwaway document. Content is nonsense filler — no
// real personal data appears in any test in this repository.
func seedSrcDoc(t *testing.T, pg *Postgres, src model.SourceType, occurredAt *time.Time) uuid.UUID {
	t.Helper()

	id := uuid.New()
	_, err := pg.pool.Exec(context.Background(), `
		INSERT INTO documents (id, source_type, source_id, title, content, embedding, occurred_at, collected_at)
		VALUES ($1, $2, $3, 'zzdummy title', 'zzdummy sentinel body', $4, $5, now())`,
		id, string(src), srcTestPrefix+id.String(),
		pgvector.NewVector([]float32{0.1, 0.2, 0.3}), occurredAt,
	)
	if err != nil {
		t.Fatalf("seed document (%s): %v", src, err)
	}
	return id
}

// TestDB_FulltextSearch_MultiSourceInclude executes the non-vector statement
// verbatim: `source_type = ANY($N)` against a []model.SourceType parameter.
func TestDB_FulltextSearch_MultiSourceInclude(t *testing.T) {
	pg := srcTestDB(t)
	store := NewDocumentStore(pg)

	gmailID := seedSrcDoc(t, pg, model.SourceGmail, nil)
	smsID := seedSrcDoc(t, pg, model.SourceSMS, nil)
	slackID := seedSrcDoc(t, pg, model.SourceSlack, nil)

	got, err := store.Search(context.Background(), model.SearchQuery{
		Query:       "zzdummy",
		Limit:       50,
		SourceTypes: []model.SourceType{model.SourceGmail, model.SourceSMS},
	})
	if err != nil {
		t.Fatalf("fulltext Search with a multi-source include filter failed: %v", err)
	}

	seen := map[uuid.UUID]bool{}
	for _, r := range got {
		seen[r.ID] = true
	}
	if !seen[gmailID] || !seen[smsID] {
		t.Errorf("include set [gmail sms] did not return both seeded documents: %v", seen)
	}
	if seen[slackID] {
		t.Errorf("document %s (slack) survived an include set that does not contain slack", slackID)
	}
}

// TestDB_HybridSearch_MultiSourceInclude executes the five-lane statement. The
// point is acceptance and predicate behaviour, not ranking.
func TestDB_HybridSearch_MultiSourceInclude(t *testing.T) {
	pg := srcTestDB(t)
	store := NewDocumentStore(pg)

	gmailID := seedSrcDoc(t, pg, model.SourceGmail, nil)
	slackID := seedSrcDoc(t, pg, model.SourceSlack, nil)

	got, err := store.Search(context.Background(), model.SearchQuery{
		Query:       "zzdummy",
		Limit:       50,
		Embedding:   []float32{0.1, 0.2, 0.3},
		SourceTypes: []model.SourceType{model.SourceGmail},
	})
	if err != nil {
		t.Fatalf("hybrid Search with a multi-source include filter failed: %v", err)
	}

	seen := map[uuid.UUID]bool{}
	for _, r := range got {
		seen[r.ID] = true
	}
	if !seen[gmailID] {
		t.Errorf("include set [gmail] did not return the seeded gmail document")
	}
	if seen[slackID] {
		t.Errorf("document %s (slack) survived an include set of [gmail]", slackID)
	}
}

// TestDB_SingularSourceType_StillFilters covers the callers that were not
// migrated to the plural field.
func TestDB_SingularSourceType_StillFilters(t *testing.T) {
	pg := srcTestDB(t)
	store := NewDocumentStore(pg)

	calID := seedSrcDoc(t, pg, model.SourceCalendar, nil)
	smsID := seedSrcDoc(t, pg, model.SourceSMS, nil)

	src := model.SourceCalendar
	got, err := store.Search(context.Background(), model.SearchQuery{
		Query:      "zzdummy",
		Limit:      50,
		SourceType: &src,
	})
	if err != nil {
		t.Fatalf("Search with the singular include filter failed: %v", err)
	}

	seen := map[uuid.UUID]bool{}
	for _, r := range got {
		seen[r.ID] = true
	}
	if !seen[calID] || seen[smsID] {
		t.Errorf("singular include filter [calendar] returned %v, want only %s", seen, calID)
	}
}

// TestDB_FilterIDsByOccurredRange is the R4 verifier against a real database:
// half-open bounds, and a NULL occurred_at never passing a window.
func TestDB_FilterIDsByOccurredRange(t *testing.T) {
	pg := srcTestDB(t)
	store := NewDocumentStore(pg)

	from := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)

	inside := from.Add(6 * time.Hour)
	before := from.Add(-6 * time.Hour)
	atUpper := to // half-open: belongs to the NEXT window

	insideID := seedSrcDoc(t, pg, model.SourceSMS, &inside)
	beforeID := seedSrcDoc(t, pg, model.SourceSMS, &before)
	upperID := seedSrcDoc(t, pg, model.SourceSMS, &atUpper)
	nullID := seedSrcDoc(t, pg, model.SourceSMS, nil)

	ids := []uuid.UUID{insideID, beforeID, upperID, nullID}

	allowed, err := store.FilterIDsByOccurredRange(context.Background(), ids, &from, &to)
	if err != nil {
		t.Fatalf("FilterIDsByOccurredRange failed: %v", err)
	}

	if _, ok := allowed[insideID]; !ok {
		t.Errorf("document inside the window was rejected")
	}
	for name, id := range map[string]uuid.UUID{
		"before the lower bound":                 beforeID,
		"exactly at the upper bound (half-open)": upperID,
		"NULL occurred_at":                       nullID,
	} {
		if _, ok := allowed[id]; ok {
			t.Errorf("document %s (%s) passed verification", id, name)
		}
	}

	// A one-sided window must constrain one side and only one side.
	allowed, err = store.FilterIDsByOccurredRange(context.Background(), ids, &from, nil)
	if err != nil {
		t.Fatalf("FilterIDsByOccurredRange (from only) failed: %v", err)
	}
	if _, ok := allowed[upperID]; !ok {
		t.Errorf("from-only window rejected a document after the lower bound")
	}
	if _, ok := allowed[beforeID]; ok {
		t.Errorf("from-only window admitted a document before the lower bound")
	}
	if _, ok := allowed[nullID]; ok {
		t.Errorf("from-only window admitted a document with no occurred_at")
	}

	// Empty input must not reach the database at all.
	empty, err := store.FilterIDsByOccurredRange(context.Background(), nil, &from, &to)
	if err != nil || len(empty) != 0 {
		t.Errorf("FilterIDsByOccurredRange(nil) = %v, %v; want an empty set and no error", empty, err)
	}
}
