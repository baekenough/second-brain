package evaldump

import (
	"strings"
	"testing"
)

func TestDecode_V2HeaderAndRows(t *testing.T) {
	src := strings.Join([]string{
		`{"kind":"header","dump_version":2,"label_hash":"lh1","config_hash":"ch1","code_revision":"rev1","attempted":2,"failed":0,"label_source":"golden-user","split":"all","window_mode":"none","created_at":"2026-09-24T00:00:00Z"}`,
		`{"kind":"query","query_id":"q1","query_source":"seed","relevant_docs":[{"doc_id":"d1","final_rank":1,"source_type":"call"}],"search_failed":false,"negative_ids_in_top10":0,"latency_ms":12.5,"ndcg10":1,"recall10":1,"fp10":0}`,
		`{"kind":"query","query_id":"q2","query_source":"seed","relevant_docs":[],"search_failed":false,"negative_ids_in_top10":1,"latency_ms":8,"ndcg10":null,"recall10":null,"fp10":1}`,
	}, "\n")

	dump, err := Decode(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if dump.Version != 2 {
		t.Fatalf("Version = %d, want 2", dump.Version)
	}
	if dump.Header == nil {
		t.Fatal("Header is nil for a v2 dump")
	}
	if dump.Header.LabelHash != "lh1" || dump.Header.ConfigHash != "ch1" || dump.Header.CodeRevision != "rev1" {
		t.Fatalf("header provenance lost: %+v", dump.Header)
	}
	if dump.Header.LabelSource != "golden-user" || dump.Header.Split != "all" || dump.Header.WindowMode != "none" {
		t.Fatalf("header metadata lost: %+v", dump.Header)
	}
	if len(dump.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(dump.Rows))
	}

	row1 := dump.Rows[0]
	if row1.Kind != "query" || row1.QueryID != "q1" {
		t.Fatalf("row1 identity wrong: %+v", row1)
	}
	if row1.LatencyMs == nil || *row1.LatencyMs != 12.5 {
		t.Fatalf("row1 latency_ms = %v, want 12.5", row1.LatencyMs)
	}
	if row1.NDCG10 == nil || *row1.NDCG10 != 1 {
		t.Fatalf("row1 ndcg10 = %v, want 1", row1.NDCG10)
	}
	if row1.FP10 == nil || *row1.FP10 != 0 {
		t.Fatalf("row1 fp10 = %v, want 0", row1.FP10)
	}
	if len(row1.RelevantDocs) != 1 || row1.RelevantDocs[0].FinalRank == nil || *row1.RelevantDocs[0].FinalRank != 1 {
		t.Fatalf("row1 relevant_docs wrong: %+v", row1.RelevantDocs)
	}

	row2 := dump.Rows[1]
	if row2.NDCG10 != nil || row2.Recall10 != nil {
		t.Fatalf("row2 (no positives) must have nil ndcg10/recall10, got %+v", row2)
	}
	if row2.FP10 == nil || *row2.FP10 != 1 {
		t.Fatalf("row2 fp10 = %v, want 1", row2.FP10)
	}
}

// A v1 dump (no header) must still parse cleanly: every line is a row, and
// the caller must be able to tell provenance is unknown from Header == nil.
func TestDecode_V1HasNoHeaderAndUnknownProvenance(t *testing.T) {
	src := strings.Join([]string{
		`{"query_id":"q1","query_source":"seed","relevant_docs":[{"doc_id":"d1","final_rank":3,"source_type":"call"}],"search_failed":false,"negative_ids_in_top10":0}`,
		`{"query_id":"q2","query_source":"seed","relevant_docs":[],"search_failed":true,"negative_ids_in_top10":0}`,
	}, "\n")

	dump, err := Decode(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if dump.Version != 1 {
		t.Fatalf("Version = %d, want 1", dump.Version)
	}
	if dump.Header != nil {
		t.Fatalf("Header must be nil for a v1 dump, got %+v", dump.Header)
	}
	if len(dump.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(dump.Rows))
	}
	if dump.Rows[0].Kind != "" {
		t.Fatalf("v1 row must have empty Kind, got %q", dump.Rows[0].Kind)
	}
	if dump.Rows[0].QueryID != "q1" {
		t.Fatalf("row1 query_id = %q, want q1", dump.Rows[0].QueryID)
	}
	if !dump.Rows[1].SearchFailed {
		t.Fatal("row2 search_failed must round-trip true")
	}
	for _, row := range dump.Rows {
		if row.LatencyMs != nil || row.NDCG10 != nil || row.Recall10 != nil || row.FP10 != nil {
			t.Fatalf("v1 row must have no v2 metric fields, got %+v", row)
		}
	}
}

func TestDecode_BlankLinesAreSkipped(t *testing.T) {
	src := "\n" +
		`{"kind":"header","dump_version":2,"label_hash":"lh","config_hash":"ch","code_revision":"rev","attempted":1,"failed":0}` + "\n" +
		"\n" +
		`{"kind":"query","query_id":"q1","relevant_docs":[]}` + "\n" +
		"  \n"

	dump, err := Decode(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if dump.Version != 2 || dump.Header == nil {
		t.Fatalf("blank lines around the header confused version detection: version=%d header=%+v", dump.Version, dump.Header)
	}
	if len(dump.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(dump.Rows))
	}
}

func TestDecode_MalformedLineErrors(t *testing.T) {
	src := `{"kind":"header","dump_version":2,"label_hash":"lh","config_hash":"ch","code_revision":"rev","attempted":1,"failed":0}` + "\n" +
		`not json` + "\n"
	if _, err := Decode(strings.NewReader(src)); err == nil {
		t.Fatal("Decode: want error on a malformed line, got nil")
	}
}

func TestDecode_EmptyInputYieldsEmptyV1Dump(t *testing.T) {
	dump, err := Decode(strings.NewReader(""))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if dump.Version != 1 || dump.Header != nil || len(dump.Rows) != 0 {
		t.Fatalf("empty input should be an empty v1 dump, got %+v", dump)
	}
}

func TestRead_MissingFile(t *testing.T) {
	if _, err := Read("/nonexistent/does-not-exist.jsonl"); err == nil {
		t.Fatal("Read: want error for a missing file, got nil")
	}
}
