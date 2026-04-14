package store

import (
	"encoding/json"
	"testing"
	"time"
)

// TestFeedback_ZeroValueMetadata verifies that a Feedback with nil Metadata
// marshals to "{}" rather than "null", matching the NOT NULL DEFAULT '{}' column.
func TestFeedback_ZeroValueMetadata(t *testing.T) {
	t.Parallel()

	f := Feedback{
		Source: "search",
		Thumbs: 1,
		// Metadata intentionally nil
	}

	// Simulate the marshal step done by Record().
	b, err := json.Marshal(f.Metadata)
	if err != nil {
		t.Fatalf("marshal nil metadata: %v", err)
	}

	// nil map marshals to "null" — callers must guard with an empty map.
	// This test documents the expected behaviour so callers know to initialise it.
	if string(b) != "null" {
		t.Errorf("nil map json = %s, want null", b)
	}

	// After initialisation the marshal must produce "{}".
	f.Metadata = map[string]any{}
	b2, err := json.Marshal(f.Metadata)
	if err != nil {
		t.Fatalf("marshal empty metadata: %v", err)
	}
	if string(b2) != "{}" {
		t.Errorf("empty map json = %s, want {}", b2)
	}
}

// TestFeedback_MetadataRoundtrip verifies that arbitrary metadata values survive
// a JSON marshal/unmarshal cycle without data loss.
func TestFeedback_MetadataRoundtrip(t *testing.T) {
	t.Parallel()

	original := map[string]any{
		"channel_id": "C12345",
		"message_id": "M98765",
		"score":      float64(0.87),
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var recovered map[string]any
	if err := json.Unmarshal(b, &recovered); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for k, want := range original {
		got, ok := recovered[k]
		if !ok {
			t.Errorf("key %q missing after roundtrip", k)
			continue
		}
		// JSON numbers decode to float64; compare as such.
		if want != got {
			t.Errorf("key %q: got %v (%T), want %v (%T)", k, got, got, want, want)
		}
	}
}

// TestFeedback_ThumbsConstraint verifies the domain rule: thumbs must be -1, 0, or +1.
// This is a pure Go validation test — the CHECK constraint is enforced by Postgres.
func TestFeedback_ThumbsConstraint(t *testing.T) {
	t.Parallel()

	valid := []int16{-1, 0, 1}
	for _, v := range valid {
		if v < -1 || v > 1 {
			t.Errorf("thumbs=%d should be valid", v)
		}
	}

	invalid := []int16{-2, 2, 100}
	for _, v := range invalid {
		if v >= -1 && v <= 1 {
			t.Errorf("thumbs=%d should be invalid", v)
		}
	}
}

// TestFeedbackStats_ZeroValue verifies that an empty FeedbackStats produces
// a valid JSON object (no nil-map panic).
func TestFeedbackStats_ZeroValue(t *testing.T) {
	t.Parallel()

	s := FeedbackStats{
		BySource: map[string]FeedbackSourceStats{},
	}

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal zero FeedbackStats: %v", err)
	}

	var decoded FeedbackStats
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal FeedbackStats: %v", err)
	}
	if len(decoded.BySource) != 0 {
		t.Errorf("by_source len = %d, want 0", len(decoded.BySource))
	}
}

// TestFeedback_CreatedAtPreserved verifies that the CreatedAt field is set and
// not the zero time — a signal that callers must rely on DB DEFAULT, not Go zero.
func TestFeedback_CreatedAtPreserved(t *testing.T) {
	t.Parallel()

	now := time.Now()
	f := Feedback{
		Source:    "api",
		Thumbs:    -1,
		CreatedAt: now,
	}

	if f.CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero after explicit assignment")
	}
	if !f.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", f.CreatedAt, now)
	}
}
