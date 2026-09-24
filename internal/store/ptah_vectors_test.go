package store

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

func ptahTestTargets() ptahVectorTargets {
	return ptahVectorTargets{
		document: &ptahVectorColumn{column: `"embedding_bge_m3"`, generation: "32b2f906929e"},
		summary:  &ptahVectorColumn{column: `"embedding_bge_m3"`, generation: "8b0ffb96cb01"},
		chunk:    &ptahVectorColumn{column: `"embedding_bge_m3"`, generation: "403c06ae7e23"},
	}
}

// Under VECTOR_SOURCE=ptah the two document vector lanes read the family
// tables Ptah created, joined back to documents so every lane filter still
// applies inside the lane, before its candidate LIMIT.
func TestBuildHybridSearchQueryFrom_PtahLanesReadTheGenerationTables(t *testing.T) {
	q, _ := buildHybridSearchQueryFrom(rangeQuery(), model.SearchWeights{}.Defaults(), documentVectorSources(ptahTestTargets()))
	lanes := laneBodies(t, q)

	cases := []struct {
		lane, from, order string
	}{
		{"vec", `FROM documents JOIN "document_vectors" v USING (id)`, `ORDER BY v."embedding_bge_m3" <=> $2 ASC`},
		{"summvec", `FROM documents JOIN "document_summary_vectors" v USING (id)`, `ORDER BY v."embedding_bge_m3" <=> $2 ASC`},
	}
	for _, tc := range cases {
		body := lanes[tc.lane]
		for _, want := range []string{tc.from, tc.order, `WHERE v."embedding_bge_m3" IS NOT NULL`, "AND status = 'active'", "occurred_at >="} {
			if !strings.Contains(body, want) {
				t.Errorf("lane %s has no %q:\n%s", tc.lane, want, body)
			}
		}
		if strings.Contains(body, "summary_embedding") || strings.Contains(body, "ORDER BY embedding") {
			t.Errorf("lane %s still reads the application's column:\n%s", tc.lane, body)
		}
	}
}

// A family with no usable generation contributes nothing, and the statement
// stays valid: the lane is an empty CTE rather than a join to a table that
// may not exist yet.
func TestBuildHybridSearchQueryFrom_LaneWithoutGenerationIsEmpty(t *testing.T) {
	targets := ptahTestTargets()
	targets.summary = nil

	q, _ := buildHybridSearchQueryFrom(rangeQuery(), model.SearchWeights{}.Defaults(), documentVectorSources(targets))
	lanes := laneBodies(t, q)

	if !strings.Contains(lanes["summvec"], "WHERE false") {
		t.Errorf("summvec lane is not empty:\n%s", lanes["summvec"])
	}
	if strings.Contains(q, "document_summary_vectors") {
		t.Errorf("statement names document_summary_vectors although it has no generation")
	}
	if !strings.Contains(lanes["vec"], `JOIN "document_vectors" v USING (id)`) {
		t.Errorf("vec lane lost its generation:\n%s", lanes["vec"])
	}
}

// The default is byte-for-byte the application's own columns.
func TestBuildHybridSearchQuery_AppSourcesReadTheApplicationColumns(t *testing.T) {
	q, _ := buildHybridSearchQuery(rangeQuery(), model.SearchWeights{}.Defaults())
	lanes := laneBodies(t, q)

	if !strings.Contains(lanes["vec"], "ORDER BY embedding <=> $2 ASC") || !strings.Contains(lanes["vec"], "FROM documents\n") {
		t.Errorf("vec lane:\n%s", lanes["vec"])
	}
	if !strings.Contains(lanes["summvec"], "ORDER BY summary_embedding <=> $2 ASC") {
		t.Errorf("summvec lane:\n%s", lanes["summvec"])
	}
}

// The chunk generation's vectors were computed from chunk_sparse_context
// text, so the chunk lane applies the rule the sparse lane applies to that
// text: a row whose fingerprint no longer matches its document is not used.
func TestPtahChunkVectorSQL_KeepsTheSparseLaneFingerprintRule(t *testing.T) {
	q := ptahChunkVectorSQL(*ptahTestTargets().chunk, " AND d.source_type = ANY($3)")

	for _, want := range []string{
		`FROM "chunk_context_vectors" v`,
		"JOIN chunk_sparse_context sc\n\t\t  ON sc.chunk_id = v.chunk_id AND sc.context_version = v.context_version",
		"v.context_version = 'v1-full'",
		"sc.fingerprint = " + chunkSparseFingerprintSQL,
		`ORDER BY v."embedding_bge_m3" <=> $1::vector`,
		"AND d.status = 'active'  AND d.source_type = ANY($3)",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("chunk statement has no %q:\n%s", want, q)
		}
	}
}

// A generation of another dimension cannot be compared with the query
// vector. Its lane goes dark rather than failing every search inside pgvector.
func TestPtahVectorResolverPick_RefusesAnotherDimension(t *testing.T) {
	r := &ptahVectorResolver{dimension: 1024, reported: map[string]string{}}
	rows := map[string]ptahPointerRow{
		PtahDocumentVectorsTable: {generation: "32b2f906929e812747b0", column: "embedding_bge_m3", dimension: 1024},
		PtahSummaryVectorsTable:  {generation: "8b0ffb96cb01f2ef7a30", column: "embedding_v3", dimension: 1536},
	}

	if got := r.pick(rows, PtahDocumentVectorsTable); got == nil || got.column != `"embedding_bge_m3"` {
		t.Errorf("document = %+v, want the quoted embedding_bge_m3 column", got)
	}
	if got := r.pick(rows, PtahSummaryVectorsTable); got != nil {
		t.Errorf("summary = %+v, want nil for a 1536-dimension generation", got)
	}
	if got := r.pick(rows, PtahChunkVectorsTable); got != nil {
		t.Errorf("chunk = %+v, want nil with no pointer row", got)
	}
}

// deploy/ptah/specs and this package describe one arrangement from two
// sides: the specifications say what Ptah embeds and where it writes, and
// search reads those tables with the application's recipes. A table renamed
// on one side, or a recipe changed on the other, would leave search reading a
// generation nothing writes, or a generation embedded from other text.
func TestPtahSpecs_MatchTheTablesAndRecipesSearchReads(t *testing.T) {
	cases := []struct {
		file   string
		values map[string]string
	}{
		{"documents.bge-m3.yaml", map[string]string{
			"source.table":            "documents",
			"source.key_fields":       "[id]",
			"source.input_fields":     "[title, content]",
			"preprocessing.separator": `"\n\n"`,
			"target.table":            PtahDocumentVectorsTable,
			"target.layout":           "own_table",
			"target.metric":           "cosine",
		}},
		{"summaries.bge-m3.yaml", map[string]string{
			"source.table":            "documents",
			"source.key_fields":       "[id]",
			"source.input_fields":     "[title_summary, bullet_summary]",
			"preprocessing.separator": `"\n\n"`,
			"target.table":            PtahSummaryVectorsTable,
			"target.layout":           "own_table",
			"target.metric":           "cosine",
		}},
		{"chunks.bge-m3.yaml", map[string]string{
			"source.table":        "chunk_sparse_context",
			"source.filter":       `"context_version = '` + ptahChunkContextVersion + `'"`,
			"source.key_fields":   "[chunk_id, context_version]",
			"source.input_fields": "[sparse_text]",
			"target.table":        PtahChunkVectorsTable,
			"target.layout":       "own_table",
			"target.metric":       "cosine",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			got := readSpecKeys(t, filepath.Join("..", "..", "deploy", "ptah", "specs", tc.file))
			for key, want := range tc.values {
				if got[key] != want {
					t.Errorf("%s = %q, want %q", key, got[key], want)
				}
			}
		})
	}
}

// readSpecKeys reads the two-level keys of a specification as
// "section.key" -> raw value. The specifications are flat enough that this
// is all a comparison needs, and it keeps a YAML library out of go.mod.
func readSpecKeys(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	keys := map[string]string{}
	section := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		switch {
		case !strings.HasPrefix(line, " "):
			section = key
		case strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   "):
			keys[section+"."+key] = strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(keys) == 0 {
		t.Fatalf("%s holds no keys", path)
	}
	return keys
}
