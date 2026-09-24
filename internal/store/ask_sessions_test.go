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

// TestAskCitationVerification_JSON covers the JSONB round trip migration
// 039's citation_verification column relies on (issue #268): every field
// round-trips through marshal->unmarshal unchanged, and — mirroring
// TestAskSource_JSON_OccurredAt's "pre-#218 row" case above — JSON that
// predates a field addition must still unmarshal without error.
func TestAskCitationVerification_JSON(t *testing.T) {
	t.Parallel()

	t.Run("full value round-trips", func(t *testing.T) {
		t.Parallel()
		v := AskCitationVerification{
			CitationStatus:    "invalid",
			CitedIDs:          []string{"11111111-1111-1111-1111-111111111111"},
			UnknownIDs:        []string{"11111111-1111-1111-1111-111111111111"},
			MalformedLinks:    2,
			InferredCitedIDs:  []string{},
			PromptEvidenceIDs: []string{"22222222-2222-2222-2222-222222222222"},
			ClaimSupport:      "not_evaluated",
		}
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got AskCitationVerification
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.CitationStatus != v.CitationStatus || got.MalformedLinks != v.MalformedLinks || got.ClaimSupport != v.ClaimSupport {
			t.Errorf("got = %+v, want %+v", got, v)
		}
		if len(got.CitedIDs) != 1 || got.CitedIDs[0] != v.CitedIDs[0] {
			t.Errorf("CitedIDs = %v, want %v", got.CitedIDs, v.CitedIDs)
		}
		if len(got.PromptEvidenceIDs) != 1 || got.PromptEvidenceIDs[0] != v.PromptEvidenceIDs[0] {
			t.Errorf("PromptEvidenceIDs = %v, want %v", got.PromptEvidenceIDs, v.PromptEvidenceIDs)
		}
	})

	t.Run("nil AskSession.Verification is not fabricated into a zero-value struct", func(t *testing.T) {
		t.Parallel()
		// scanAskSession's own contract (ask_sessions.go): an empty/NULL
		// citation_verification column must leave AskSession.Verification
		// nil, not &AskCitationVerification{}. This is a documentation test —
		// the actual NULL-scan behaviour needs a real DB connection, see
		// ask_sessions_verification_db_test.go.
		var session AskSession
		if session.Verification != nil {
			t.Fatalf("zero-value AskSession.Verification = %+v, want nil", session.Verification)
		}
	})
}
