// Package evaldump reads the JSON Lines dump files written by cmd/eval's
// --dump flag (cmd/eval/diagnostics.go). It never opens a network connection
// or a database — it only parses bytes already on disk, so it is safe to use
// from CI and from cmd/evalcompare without any external dependency.
//
// Two dump shapes exist:
//
//   - v1: no header line. Every line is a query row (the shape that shipped
//     before issue #269). Provenance (label/config hash, code revision) is
//     unknown, because it was never written to the file — it lived only in
//     the eval_metrics row for that run.
//   - v2: line 1 is a header object ("kind":"header") carrying that
//     provenance, and each following line is a query row ("kind":"query")
//     that additionally carries latency_ms/ndcg10/recall10/fp10.
//
// Read distinguishes the two by peeking at the first line's "kind" field, so
// a v1 file (whose first line has no such field) is read whole as query rows.
package evaldump

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// SchemaVersion is the dump_version written by the current cmd/eval.
// internal/evalcompare compares this against Dump.Version, not a hardcoded
// literal, so the two packages cannot silently drift apart.
const SchemaVersion = 2

// Header is line 1 of a v2 dump. It is the run's provenance: which labels,
// which search configuration, and which code produced every row that
// follows. cmd/eval builds it from the same configHash/labelHash/revision
// values it already writes to the eval_metrics table (see
// cmd/eval/main.go's metricsStore.Save call) — the header adds no new
// concept, only makes the existing provenance travel with the dump file.
type Header struct {
	// Kind is always "header". It is what tells Read that this file is a v2
	// dump rather than a v1 file whose first line is already a query row.
	Kind        string `json:"kind"`
	DumpVersion int    `json:"dump_version"`

	LabelHash    string `json:"label_hash"`
	ConfigHash   string `json:"config_hash"`
	CodeRevision string `json:"code_revision"`

	Attempted int `json:"attempted"`
	Failed    int `json:"failed"`

	// LabelSource is "golden-user" (--golden) or "feedback". Split, and
	// WindowMode mirror the flags the run was invoked with; they are
	// provenance for a human reading a comparison report, not inputs the
	// comparator itself branches on beyond the label/config hash checks.
	LabelSource string `json:"label_source,omitempty"`
	Split       string `json:"split,omitempty"`
	WindowMode  string `json:"window_mode,omitempty"`

	// CreatedAt is RFC3339, UTC. It is informational only — no comparison
	// logic depends on it.
	CreatedAt string `json:"created_at,omitempty"`
}

// RelevantDoc is the subset of cmd/eval's per-label diagnostic fields that
// evalcompare needs: which slice a label document belongs to (SourceType),
// and the rank evidence (FinalRank) used to cross-check a row's own ndcg10
// against an independent recomputation.
type RelevantDoc struct {
	DocID string `json:"doc_id"`
	// FinalRank is the label document's 1-based rank in the row's results,
	// or nil when it was not retrieved at all.
	FinalRank  *int   `json:"final_rank"`
	SourceType string `json:"source_type"`
}

// Row is one query's diagnostic + metric line. In a v2 dump, Kind is
// "query"; in a v1 dump, Kind is empty because the field never existed.
type Row struct {
	Kind        string `json:"kind"`
	QueryID     string `json:"query_id"`
	QuerySource string `json:"query_source"`

	RelevantDocs []RelevantDoc `json:"relevant_docs"`

	SearchFailed bool `json:"search_failed"`
	// NegativeInTop10 is the same count cmd/eval always wrote
	// (negative_ids_in_top10). FP10 mirrors it under the dump-v2 metric name
	// so a v2 reader does not need to know the v1 field name.
	NegativeInTop10 int `json:"negative_ids_in_top10"`

	// The four fields below are v2-only: nil on every row of a v1 dump.
	// Within a v2 dump, NDCG10/Recall10 are additionally nil for a query
	// with no positive labels — 0 and "not scoreable" are different facts.
	LatencyMs *float64 `json:"latency_ms"`
	NDCG10    *float64 `json:"ndcg10"`
	Recall10  *float64 `json:"recall10"`
	FP10      *int     `json:"fp10"`
}

// Dump is a fully parsed eval dump file.
type Dump struct {
	// Version is 1 or 2. A v1 Dump always has Header == nil — callers must
	// treat that as "provenance unknown", never as "provenance matches".
	Version int
	Header  *Header
	Rows    []Row
}

// kindPeek is decoded first, on every line, to route it to Header or Row
// without guessing from field shape.
type kindPeek struct {
	Kind string `json:"kind"`
}

// Read opens path and parses it as an eval dump (v1 or v2).
func Read(path string) (*Dump, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("evaldump: open %s: %w", path, err)
	}
	defer f.Close()
	dump, err := Decode(f)
	if err != nil {
		return nil, fmt.Errorf("evaldump: %s: %w", path, err)
	}
	return dump, nil
}

// Decode parses dump content from r. Split out from Read so tests and
// in-memory callers do not need a real file on disk.
func Decode(r io.Reader) (*Dump, error) {
	scanner := bufio.NewScanner(r)
	// Dump rows never carry query/document text (see
	// cmd/eval/diagnostics.go), but a corpus with many labels per query can
	// still produce a long line; the default 64KiB bufio limit is too easy
	// to hit for a file this package must never refuse to read.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	dump := &Dump{Version: 1}
	lineNo := 0
	first := true
	for scanner.Scan() {
		lineNo++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var peek kindPeek
		if err := json.Unmarshal(line, &peek); err != nil {
			return nil, fmt.Errorf("parse line %d: %w", lineNo, err)
		}

		if first && peek.Kind == "header" {
			var h Header
			if err := json.Unmarshal(line, &h); err != nil {
				return nil, fmt.Errorf("parse header (line %d): %w", lineNo, err)
			}
			dump.Header = &h
			dump.Version = h.DumpVersion
			if dump.Version == 0 {
				dump.Version = SchemaVersion
			}
			first = false
			continue
		}
		first = false

		var row Row
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, fmt.Errorf("parse row (line %d): %w", lineNo, err)
		}
		dump.Rows = append(dump.Rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	return dump, nil
}
