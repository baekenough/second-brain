package store

import (
	"encoding/json"
	"testing"
	"time"
)

// TestAskSource_JSON_OccurredAt covers the JSONB round trip (AskSession.Sources
// marshals straight into the sources column, migration 024 — see
// AskSource's doc comment) that Insert/scanAskSession rely on, without a real
// Postgres connection: a set OccurredAt must survive marshal->unmarshal
// unchanged, a nil OccurredAt must marshal to a literal JSON null (not be
// omitted — issue #218's whole point is that "no event time" and "field
// never shipped" must be visibly different on the wire this struct also
// backs), and JSON that predates this field (no "occurred_at" key at all,
// simulating a pre-#218 row) must unmarshal to nil rather than error.
func TestAskSource_JSON_OccurredAt(t *testing.T) {
	t.Parallel()
	occurredAt := time.Date(2026, 8, 19, 10, 15, 0, 0, time.UTC)

	t.Run("set value round-trips", func(t *testing.T) {
		t.Parallel()
		src := AskSource{ID: "d1", Title: "t", SourceType: "sms", Score: 0.9, OccurredAt: &occurredAt}
		raw, err := json.Marshal(src)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got AskSource
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.OccurredAt == nil || !got.OccurredAt.Equal(occurredAt) {
			t.Errorf("OccurredAt = %v, want %s", got.OccurredAt, occurredAt)
		}
	})

	t.Run("nil marshals to literal null, not an omitted key", func(t *testing.T) {
		t.Parallel()
		src := AskSource{ID: "d2", Title: "t", SourceType: "gmail", Score: 0.5}
		raw, err := json.Marshal(src)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("unmarshal into map: %v", err)
		}
		val, ok := fields["occurred_at"]
		if !ok {
			t.Fatalf("occurred_at key missing entirely; raw=%s", raw)
		}
		if string(val) != "null" {
			t.Errorf("occurred_at = %s, want literal null", val)
		}
	})

	t.Run("pre-#218 JSON with no occurred_at key unmarshals to nil", func(t *testing.T) {
		t.Parallel()
		legacy := []byte(`{"id":"d3","title":"t","source_type":"whisper","score":0.7}`)
		var got AskSource
		if err := json.Unmarshal(legacy, &got); err != nil {
			t.Fatalf("unmarshal legacy row: %v", err)
		}
		if got.OccurredAt != nil {
			t.Errorf("OccurredAt = %v, want nil for a row that predates this field", got.OccurredAt)
		}
	})
}
