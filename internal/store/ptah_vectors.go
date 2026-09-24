package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Ptah generations as the source of search vectors (VECTOR_SOURCE=ptah).
//
// deploy/ptah builds one generation per vector family beside the
// application's own columns, each in a table Ptah creates and owns:
//
//	document_vectors          title + "\n\n" + content          the vec lane
//	document_summary_vectors  title_summary + bullet_summary    the summvec lane
//	chunk_context_vectors     chunk_sparse_context v1-full      chunk search
//
// `ptah inference cutover` records which generation a table serves in
// ptah_embedding_pointer, and ptah_embedding_generation names the column that
// generation wrote. Search reads those two tables instead of a column name in
// configuration, so a cutover or a rollback reaches search on the next
// resolve, without a deploy.
//
// The pointer is kept per target table. That is why the three families have
// three tables: two families sharing one table would share one pointer, and
// cutting one over would take the other off the air.
const (
	PtahDocumentVectorsTable = "document_vectors"
	PtahSummaryVectorsTable  = "document_summary_vectors"
	PtahChunkVectorsTable    = "chunk_context_vectors"

	// ptahChunkContextVersion is the chunk_sparse_context recipe the chunk
	// generation embeds. v1-full is the chunk-ctx-v1 embedding input: the
	// same header scheduler.withChunkContextHeader builds, then the chunk.
	ptahChunkContextVersion = "v1-full"
)

// ptahResolveInterval bounds how long a cutover or a rollback takes to reach
// search.
const ptahResolveInterval = 30 * time.Second

// ptahVectorColumn is one family's active generation, as search reads it.
type ptahVectorColumn struct {
	// column is the vector column in the family's table, already quoted.
	column string
	// generation is the generation identity, for logs.
	generation string
}

// ptahVectorTargets is what search reads for each family. A nil entry is a
// family with no usable generation, and its lane contributes nothing.
type ptahVectorTargets struct {
	document *ptahVectorColumn
	summary  *ptahVectorColumn
	chunk    *ptahVectorColumn
}

// ptahVectorResolver reads the active generations from Ptah's pointer and
// caches them for ptahResolveInterval.
type ptahVectorResolver struct {
	pg *Postgres
	// dimension is the query embedder's dimension. A generation of any
	// other dimension cannot be compared with the query vector, so its lane
	// is turned off rather than left to fail inside pgvector.
	dimension int

	mu       sync.Mutex
	cached   ptahVectorTargets
	loadedAt time.Time
	// reported remembers the last state logged per table, so a steady state
	// is logged once rather than on every resolve.
	reported map[string]string
}

// UsePtahVectors makes search read the Ptah generations described above.
// dimension is the dimension of the vectors the query embedder produces.
func (pg *Postgres) UsePtahVectors(dimension int) {
	pg.ptahVectors = &ptahVectorResolver{pg: pg, dimension: dimension, reported: map[string]string{}}
}

// targets returns the active generations, re-reading the pointer once the
// cached answer is older than ptahResolveInterval.
func (r *ptahVectorResolver) targets(ctx context.Context) ptahVectorTargets {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.loadedAt.IsZero() && time.Since(r.loadedAt) < ptahResolveInterval {
		return r.cached
	}
	rows, err := r.load(ctx)
	if err != nil {
		// A failed read keeps the last answer. Search with a known-good
		// generation is better than search with none, and the next resolve
		// tries again.
		slog.Warn("ptah vectors: cannot read the active generations; keeping the previous answer", "error", err)
		return r.cached
	}
	r.cached = ptahVectorTargets{
		document: r.pick(rows, PtahDocumentVectorsTable),
		summary:  r.pick(rows, PtahSummaryVectorsTable),
		chunk:    r.pick(rows, PtahChunkVectorsTable),
	}
	r.loadedAt = time.Now()
	return r.cached
}

// ptahPointerRow is one active generation read from the pointer.
type ptahPointerRow struct {
	generation string
	column     string
	dimension  int
}

const ptahActiveGenerationsSQL = `
	SELECT p.target_table, g.identity, g.target_column, g.dimension
	FROM ptah_embedding_pointer p
	JOIN ptah_embedding_generation g ON g.identity = p.active_generation
	WHERE p.target_schema IN ('', 'public')
	  AND p.target_table = ANY($1)
	  AND g.retired_at IS NULL`

// load reads the pointer. Tables Ptah has not created yet read as no
// generation at all, which is the state before the first `ptah inference
// prepare`.
func (r *ptahVectorResolver) load(ctx context.Context) (map[string]ptahPointerRow, error) {
	tables := []string{PtahDocumentVectorsTable, PtahSummaryVectorsTable, PtahChunkVectorsTable}
	rows, err := r.pg.pool.Query(ctx, ptahActiveGenerationsSQL, tables)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return map[string]ptahPointerRow{}, nil
		}
		return nil, fmt.Errorf("read ptah_embedding_pointer: %w", err)
	}
	defer rows.Close()
	found := map[string]ptahPointerRow{}
	for rows.Next() {
		var table string
		var row ptahPointerRow
		if err := rows.Scan(&table, &row.generation, &row.column, &row.dimension); err != nil {
			return nil, fmt.Errorf("scan ptah_embedding_pointer: %w", err)
		}
		found[table] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read ptah_embedding_pointer: %w", err)
	}
	return found, nil
}

// pick turns one table's pointer row into what search reads, or nil when the
// table has no generation search can compare with the query vector.
func (r *ptahVectorResolver) pick(rows map[string]ptahPointerRow, table string) *ptahVectorColumn {
	row, ok := rows[table]
	var state string
	var picked *ptahVectorColumn
	switch {
	case !ok:
		state = "no active generation; the lane is off"
	case r.dimension > 0 && row.dimension != r.dimension:
		state = fmt.Sprintf("generation %s has dimension %d and the query embedder produces %d; the lane is off",
			shortGeneration(row.generation), row.dimension, r.dimension)
	default:
		state = fmt.Sprintf("reading generation %s from %s.%s", shortGeneration(row.generation), table, row.column)
		picked = &ptahVectorColumn{
			column:     pgx.Identifier{row.column}.Sanitize(),
			generation: row.generation,
		}
	}
	if r.reported[table] != state {
		r.reported[table] = state
		slog.Info("ptah vectors: "+state, "table", table)
	}
	return picked
}

func shortGeneration(identity string) string {
	if len(identity) > 12 {
		return identity[:12]
	}
	return identity
}

// documentVectorSources renders where the two document lanes read from.
func documentVectorSources(t ptahVectorTargets) vectorSources {
	return vectorSources{
		document: ptahDocumentLane(PtahDocumentVectorsTable, t.document),
		summary:  ptahDocumentLane(PtahSummaryVectorsTable, t.summary),
	}
}

// ptahDocumentLane joins a family table to documents on the document key.
// USING (id) merges the key, so the lane filters, which name documents
// columns without a qualifier, read the same as they do against documents
// alone.
func ptahDocumentLane(table string, target *ptahVectorColumn) vectorLane {
	if target == nil {
		return vectorLane{}
	}
	return vectorLane{
		from:   "documents JOIN " + pgx.Identifier{table}.Sanitize() + " v USING (id)",
		column: "v." + target.column,
	}
}

// ptahChunkVectorSQL is chunk search over the chunk generation.
//
// The fingerprint condition is the one the sparse lane applies
// (chunk_sparse_search.go): a context row written before the document's title,
// metadata or redaction changed is not used until cmd/sparsectx --sweep
// rewrites it. The vector was computed from that row's text, so the same rule
// keeps a name removed by redaction from steering retrieval through it.
func ptahChunkVectorSQL(target ptahVectorColumn, filters string) string {
	column := "v." + target.column
	return `
		SELECT
			c.id,
			c.document_id,
			c.chunk_index,
			c.content,
			c.byte_size,
			c.created_at,
			1 - (` + column + ` <=> $1::vector)  AS score,
			d.title          AS document_title,
			d.source_type    AS document_source,
			d.status         AS document_status,
			d.occurred_at    AS document_occurred_at,
			d.collected_at   AS document_collected_at,
			d.metadata       AS document_metadata
		FROM ` + pgx.Identifier{PtahChunkVectorsTable}.Sanitize() + ` v
		JOIN chunk_sparse_context sc
		  ON sc.chunk_id = v.chunk_id AND sc.context_version = v.context_version
		JOIN chunks c ON c.id = v.chunk_id
		JOIN documents d ON d.id = c.document_id
		WHERE v.context_version = '` + ptahChunkContextVersion + `'
		  AND ` + column + ` IS NOT NULL
		  AND sc.fingerprint = ` + chunkSparseFingerprintSQL + `
		  AND d.status = 'active' ` + filters + `
		ORDER BY ` + column + ` <=> $1::vector
		LIMIT $2`
}
