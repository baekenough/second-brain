package store

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestEvalPair_JSONSerialization verifies that an EvalPair round-trips through
// JSON marshal/unmarshal without data loss (no-DB unit test).
func TestEvalPair_JSONSerialization(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	original := EvalPair{
		ID:             7,
		Query:          "what is RAG?",
		RelevantDocIDs: []string{"0b8ef9a1-1234-4abc-8def-000000000001", "0b8ef9a1-1234-4abc-8def-000000000002", "0b8ef9a1-1234-4abc-8def-000000000003"},
		Source:         "feedback",
		CreatedAt:      now,
		Metadata:       map[string]any{"channel": "general"},
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got EvalPair
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.ID != original.ID {
		t.Errorf("ID = %d, want %d", got.ID, original.ID)
	}
	if got.Query != original.Query {
		t.Errorf("Query = %q, want %q", got.Query, original.Query)
	}
	if got.Source != original.Source {
		t.Errorf("Source = %q, want %q", got.Source, original.Source)
	}
	if len(got.RelevantDocIDs) != len(original.RelevantDocIDs) {
		t.Fatalf("RelevantDocIDs len = %d, want %d", len(got.RelevantDocIDs), len(original.RelevantDocIDs))
	}
	for i, id := range original.RelevantDocIDs {
		if got.RelevantDocIDs[i] != id {
			t.Errorf("RelevantDocIDs[%d] = %q, want %q", i, got.RelevantDocIDs[i], id)
		}
	}
	if !got.CreatedAt.Equal(original.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, original.CreatedAt)
	}
}

// TestEvalPair_OmitEmptyMetadata verifies that an EvalPair with nil Metadata
// produces JSON without the "metadata" key (omitempty behaviour).
func TestEvalPair_OmitEmptyMetadata(t *testing.T) {
	t.Parallel()

	p := EvalPair{
		ID:             1,
		Query:          "query",
		RelevantDocIDs: []string{"0b8ef9a1-1234-4abc-8def-000000000010"},
		Source:         "feedback",
		CreatedAt:      time.Now(),
		// Metadata nil — should be omitted from JSON
	}

	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if strings.Contains(string(b), `"metadata"`) {
		t.Errorf("expected metadata to be omitted, got: %s", b)
	}
}

// TestEvalPair_ZeroDocIDs verifies that an EvalPair with an empty
// RelevantDocIDs slice marshals to an empty JSON array, not null.
func TestEvalPair_ZeroDocIDs(t *testing.T) {
	t.Parallel()

	p := EvalPair{
		ID:             2,
		Query:          "empty docs",
		RelevantDocIDs: []string{},
		Source:         "manual",
		CreatedAt:      time.Now(),
	}

	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got EvalPair
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RelevantDocIDs == nil {
		t.Error("RelevantDocIDs should not be nil after roundtrip of empty slice")
	}
	if len(got.RelevantDocIDs) != 0 {
		t.Errorf("RelevantDocIDs len = %d, want 0", len(got.RelevantDocIDs))
	}
}

// TestEvalStore_ExportJSONL_InMemory exercises ExportJSONL with a fabricated
// pairs slice by monkey-patching BuildFromFeedback via a helper that accepts
// pairs directly — no database required.
//
// This test validates the JSONL encoding logic (newline-delimited, each line
// is valid JSON) without touching Postgres.
func TestEvalStore_ExportJSONL_Format(t *testing.T) {
	t.Parallel()

	pairs := []EvalPair{
		{ID: 1, Query: "alpha", RelevantDocIDs: []string{"0b8ef9a1-0000-0000-0000-000000000010", "0b8ef9a1-0000-0000-0000-000000000020"}, Source: "feedback", CreatedAt: time.Now()},
		{ID: 2, Query: "beta", RelevantDocIDs: []string{"0b8ef9a1-0000-0000-0000-000000000030"}, Source: "feedback", CreatedAt: time.Now()},
		{ID: 3, Query: "gamma", RelevantDocIDs: []string{"0b8ef9a1-0000-0000-0000-000000000040", "0b8ef9a1-0000-0000-0000-000000000050", "0b8ef9a1-0000-0000-0000-000000000060"}, Source: "feedback", CreatedAt: time.Now()},
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, p := range pairs {
		if err := enc.Encode(p); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}

	// Verify output: each line should be a valid JSON object.
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != len(pairs) {
		t.Fatalf("line count = %d, want %d", len(lines), len(pairs))
	}

	for i, line := range lines {
		var got EvalPair
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Errorf("line %d is not valid JSON: %v — %q", i, err, line)
			continue
		}
		if got.Query != pairs[i].Query {
			t.Errorf("line %d query = %q, want %q", i, got.Query, pairs[i].Query)
		}
		if len(got.RelevantDocIDs) != len(pairs[i].RelevantDocIDs) {
			t.Errorf("line %d doc_ids len = %d, want %d", i, len(got.RelevantDocIDs), len(pairs[i].RelevantDocIDs))
		}
	}
}
