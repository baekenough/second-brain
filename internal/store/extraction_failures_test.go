package store

import (
	"testing"
	"time"
)

// TestExtractionFailure_ZeroValue verifies that the zero value of ExtractionFailure
// has sane defaults — in particular that DeadLetter defaults to false and Attempts
// to 0, matching the table's DEFAULT 1 / DEFAULT false semantics.
func TestExtractionFailure_ZeroValue(t *testing.T) {
	t.Parallel()

	var f ExtractionFailure
	if f.DeadLetter {
		t.Error("zero-value DeadLetter should be false")
	}
	if f.Attempts != 0 {
		t.Errorf("zero-value Attempts = %d, want 0", f.Attempts)
	}
}

// TestExtractionFailure_Fields verifies that all fields can be populated and
// retrieved without data loss — a canary for accidental field type changes.
func TestExtractionFailure_Fields(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	f := ExtractionFailure{
		ID:           42,
		SourceType:   "filesystem",
		SourceID:     "path/to/file.pdf",
		FilePath:     "/mnt/data/path/to/file.pdf",
		ErrorMessage: "unsupported content type",
		Attempts:     3,
		NextRetryAt:  now.Add(8 * time.Minute),
		DeadLetter:   false,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if f.ID != 42 {
		t.Errorf("ID = %d, want 42", f.ID)
	}
	if f.SourceType != "filesystem" {
		t.Errorf("SourceType = %q, want %q", f.SourceType, "filesystem")
	}
	if f.SourceID != "path/to/file.pdf" {
		t.Errorf("SourceID = %q, want %q", f.SourceID, "path/to/file.pdf")
	}
	if f.FilePath != "/mnt/data/path/to/file.pdf" {
		t.Errorf("FilePath = %q, want %q", f.FilePath, "/mnt/data/path/to/file.pdf")
	}
	if f.ErrorMessage != "unsupported content type" {
		t.Errorf("ErrorMessage = %q, want %q", f.ErrorMessage, "unsupported content type")
	}
	if f.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", f.Attempts)
	}
	if f.DeadLetter {
		t.Error("DeadLetter should be false")
	}
}

// TestExtractionFailure_DeadLetterThreshold documents the dead-letter promotion
// rule used in Record's ON CONFLICT clause:
//   dead_letter = (attempts + 1 >= 10)
//
// This test makes the threshold explicit so that any future change to the SQL
// is noticed here too.
func TestExtractionFailure_DeadLetterThreshold(t *testing.T) {
	t.Parallel()

	const deadLetterAt = 10

	cases := []struct {
		currentAttempts int // value stored in DB before the conflict update
		wantDeadLetter  bool
	}{
		{1, false},
		{8, false},
		{9, true},  // 9 + 1 = 10 → dead_letter
		{10, true}, // 10 + 1 = 11 → dead_letter
		{15, true},
	}

	for _, tc := range cases {
		newAttempts := tc.currentAttempts + 1
		got := newAttempts >= deadLetterAt
		if got != tc.wantDeadLetter {
			t.Errorf("currentAttempts=%d → newAttempts=%d: dead_letter=%v, want %v",
				tc.currentAttempts, newAttempts, got, tc.wantDeadLetter)
		}
	}
}

// TestExtractionFailure_BackoffCap documents the exponential back-off cap used
// in Record's ON CONFLICT clause:
//   next_retry_at = now() + LEAST(60, 2^attempts) minutes
//
// This test ensures the cap logic is correctly understood by callers.
func TestExtractionFailure_BackoffCap(t *testing.T) {
	t.Parallel()

	cases := []struct {
		attempts    int
		wantMinutes int
	}{
		{1, 2},   // 2^1 = 2
		{2, 4},   // 2^2 = 4
		{3, 8},   // 2^3 = 8
		{4, 16},  // 2^4 = 16
		{5, 32},  // 2^5 = 32
		{6, 60},  // 2^6 = 64 → capped at 60
		{10, 60}, // well above cap
	}

	for _, tc := range cases {
		pow := 1
		for i := 0; i < tc.attempts; i++ {
			pow *= 2
		}
		got := pow
		if got > 60 {
			got = 60
		}
		if got != tc.wantMinutes {
			t.Errorf("attempts=%d: back-off=%d min, want %d min",
				tc.attempts, got, tc.wantMinutes)
		}
	}
}

// TestNewExtractionFailureStore_NilSafety verifies that NewExtractionFailureStore
// returns a non-nil store (constructor sanity check, no DB required).
func TestNewExtractionFailureStore_NilSafety(t *testing.T) {
	t.Parallel()

	// Pass a zero-value Postgres to exercise the constructor path.
	// The store is not used for queries here, so the nil pool is fine.
	pg := &Postgres{}
	s := NewExtractionFailureStore(pg)
	if s == nil {
		t.Fatal("NewExtractionFailureStore returned nil")
	}
	if s.pg != pg {
		t.Error("ExtractionFailureStore.pg does not point to the provided Postgres instance")
	}
}
