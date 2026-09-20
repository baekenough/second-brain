package store

import (
	"context"
	"testing"
)

// Compare approximate search with exact search on a deterministic filtered
// corpus. The nearest 300 vectors are inactive; valid candidates lie beyond
// the default ef_search frontier. Temporary objects never touch documents.
func TestDB_HNSWFilteredRecall(t *testing.T) {
	pg := srcTestDB(t)
	ctx := context.Background()
	conn, err := pg.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var mode, ef string
	if err := conn.QueryRow(ctx, `SELECT current_setting('hnsw.iterative_scan'),current_setting('hnsw.ef_search')`).Scan(&mode, &ef); err != nil {
		t.Fatal(err)
	}
	if mode != "strict_order" || ef != "100" {
		t.Fatalf("HNSW defaults = %q/%q", mode, ef)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, sql := range []string{
		`CREATE TEMP TABLE recall_fixture(id int, active boolean, embedding vector(3)) ON COMMIT DROP`,
		`INSERT INTO recall_fixture SELECT i,i>300,ARRAY[1.0,i::real/1000,0.0]::vector FROM generate_series(1,1000) i`,
		`CREATE INDEX ON recall_fixture USING hnsw(embedding vector_cosine_ops)`,
		`SET LOCAL enable_seqscan=off`,
	} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	query := `SELECT id FROM recall_fixture WHERE active ORDER BY embedding <=> '[1,0,0]'::vector LIMIT 15`
	read := func() []int {
		rows, err := tx.Query(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ids []int
		for rows.Next() {
			var id int
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	approx := read()
	if _, err := tx.Exec(ctx, `SET LOCAL hnsw.iterative_scan=off; SET LOCAL hnsw.ef_search=40`); err != nil {
		t.Fatal(err)
	}
	baseline := read()
	t.Logf("filtered HNSW results: default=%d tuned=%d target=15", len(baseline), len(approx))
	if len(baseline) >= len(approx) {
		t.Fatalf("fixture did not exercise candidate exhaustion: baseline=%v tuned=%v", baseline, approx)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan=on; SET LOCAL enable_indexscan=off; SET LOCAL enable_bitmapscan=off`); err != nil {
		t.Fatal(err)
	}
	// Use a distinct statement so pgx cannot reuse the prepared HNSW plan.
	query += " /* exact baseline */"
	exact := read()
	if len(approx) != 15 || len(exact) != 15 {
		t.Fatalf("approx=%v exact=%v", approx, exact)
	}
	for i := range exact {
		if approx[i] != exact[i] {
			t.Fatalf("filtered recall differs: approx=%v exact=%v", approx, exact)
		}
	}
}
