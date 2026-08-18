package briefing

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestSafeLogFieldsLeaksNothing is the guard on issue #194's failure mode. The
// briefing's inputs and outputs are restatements of the user's own
// conversations, so anything this function emits ends up in container logs and
// whatever collects them. It must therefore be structurally incapable of
// carrying text — only counts, flags, and a truncated key prefix.
func TestSafeLogFieldsLeaksNothing(t *testing.T) {
	in := Input{
		Model:            "dummy-model",
		LatestDocumentAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		Actions: []InputAction{{
			IdentityKey:     "action:0123456789abcdef",
			DocumentID:      "11111111-1111-1111-1111-111111111111",
			Kind:            "my_commitment",
			Summary:         "SENSITIVE_SUMMARY_TOKEN",
			CounterpartName: "SENSITIVE_NAME_TOKEN",
			DetectedBy:      "structural",
		}},
	}
	res := Result{
		Sentences: []Sentence{{
			Text:         "SENSITIVE_SENTENCE_TOKEN",
			DocumentIDs:  []string{"11111111-1111-1111-1111-111111111111"},
			IdentityKeys: []string{"action:0123456789abcdef"},
		}},
		DroppedCount: 2,
	}

	joined := fmt.Sprint(SafeLogFields(in, res)...)

	for _, banned := range []string{"SENSITIVE_SUMMARY_TOKEN", "SENSITIVE_NAME_TOKEN", "SENSITIVE_SENTENCE_TOKEN"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("log fields leaked %q", banned)
		}
	}
	if !strings.Contains(joined, "action:0123") {
		t.Fatalf("expected truncated key prefix for correlation, got: %s", joined)
	}
	if strings.Contains(joined, "0123456789abcdef") {
		t.Fatalf("full identity key must be truncated, got: %s", joined)
	}
	if !strings.Contains(joined, "2") {
		t.Fatalf("dropped count is missing from the log fields: %s", joined)
	}
}

// TestSafeLogFieldsIsEvenLength pins that the result is usable as slog's
// alternating key/value varargs. An odd length makes slog emit a "!BADKEY"
// record, which in the worst case is the one record that would have explained a
// production failure.
func TestSafeLogFieldsIsEvenLength(t *testing.T) {
	got := SafeLogFields(Input{}, Result{})
	if len(got)%2 != 0 {
		t.Fatalf("SafeLogFields returned %d values; slog needs key/value pairs", len(got))
	}
}

func TestKeyPrefix(t *testing.T) {
	cases := map[string]string{
		"action:0123456789abcdef": "action:0123",
		// Shorter than the prefix budget: nothing to truncate.
		"action:ff": "action:ff",
		"":          "",
		"malformed": "malformed",
		// Anything longer is cut to the same 11-character budget, whatever its
		// shape, so a malformed key cannot log more than a well-formed one.
		"malformed-but-long": "malformed-b",
	}
	for in, want := range cases {
		if got := KeyPrefix(in); got != want {
			t.Errorf("KeyPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestKeyPrefixHandlesMultibyte pins that truncation is rune-safe. Slicing bytes
// out of a multi-byte value would produce invalid UTF-8 in the log stream.
func TestKeyPrefixHandlesMultibyte(t *testing.T) {
	got := KeyPrefix("액션키입니다")
	if !strings.ContainsRune(got, '액') && got != "" {
		t.Fatalf("KeyPrefix mangled a multi-byte value: %q", got)
	}
	if !isValidUTF8(got) {
		t.Fatalf("KeyPrefix produced invalid UTF-8: %q", got)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
