package store

import (
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// TestCheckSourceTypeGuard_Container verifies that a container source_type
// (secretary) increments only the container counter, and that the
// deprecated/unknown counters stay at zero.
func TestCheckSourceTypeGuard_Container(t *testing.T) {
	resetSourceGuardCountersForTest()

	checkSourceTypeGuard(&model.Document{SourceType: model.SourceSecretary})

	container, deprecated, unknown := SourceTypeGuardStats()
	if container != 1 {
		t.Errorf("container hits = %d, want 1", container)
	}
	if deprecated != 0 || unknown != 0 {
		t.Errorf("deprecated/unknown hits = %d/%d, want 0/0", deprecated, unknown)
	}
}

// TestCheckSourceTypeGuard_Deprecated verifies that a deprecated source_type
// (llm-memory) increments only the deprecated counter.
func TestCheckSourceTypeGuard_Deprecated(t *testing.T) {
	resetSourceGuardCountersForTest()

	checkSourceTypeGuard(&model.Document{
		SourceType: model.SourceLLMMemory,
		SourceID:   "zz-test-source-id",
		Metadata:   map[string]any{"kind": "session", "device_id": "macmini"},
	})

	container, deprecated, unknown := SourceTypeGuardStats()
	if deprecated != 1 {
		t.Errorf("deprecated hits = %d, want 1", deprecated)
	}
	if container != 0 || unknown != 0 {
		t.Errorf("container/unknown hits = %d/%d, want 0/0", container, unknown)
	}
}

// TestCheckSourceTypeGuard_Unknown verifies that a source_type outside
// model.KnownSourceTypes and not a container/deprecated type increments only
// the unknown counter. SourceSlack is used because it is a declared
// model.SourceType const that is intentionally NOT in KnownSourceTypes (see
// that function's doc comment — dormant integrations).
func TestCheckSourceTypeGuard_Unknown(t *testing.T) {
	resetSourceGuardCountersForTest()

	checkSourceTypeGuard(&model.Document{SourceType: model.SourceSlack})

	container, deprecated, unknown := SourceTypeGuardStats()
	if unknown != 1 {
		t.Errorf("unknown hits = %d, want 1", unknown)
	}
	if container != 0 || deprecated != 0 {
		t.Errorf("container/deprecated hits = %d/%d, want 0/0", container, deprecated)
	}
}

// TestCheckSourceTypeGuard_KnownTypesDoNotFire verifies that every known
// source_type is a complete no-op: no counter moves. This is the guard's
// most important behaviour — false positives on legitimate writes would
// flood logs and erode trust in the warning.
func TestCheckSourceTypeGuard_KnownTypesDoNotFire(t *testing.T) {
	for _, st := range model.KnownSourceTypes() {
		st := st
		t.Run(string(st), func(t *testing.T) {
			resetSourceGuardCountersForTest()

			checkSourceTypeGuard(&model.Document{SourceType: st})

			container, deprecated, unknown := SourceTypeGuardStats()
			if container != 0 || deprecated != 0 || unknown != 0 {
				t.Errorf("known source_type %q fired a guard: container=%d deprecated=%d unknown=%d",
					st, container, deprecated, unknown)
			}
		})
	}
}

// TestMetadataStringField covers the nil-map, missing-key, wrong-type, and
// happy-path branches of metadataStringField — the helper that keeps
// checkSourceTypeGuard's deprecated-type log line from panicking on
// unexpected metadata shapes.
func TestMetadataStringField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		m    map[string]any
		key  string
		want string
	}{
		{"nil map", nil, "kind", ""},
		{"missing key", map[string]any{"other": "x"}, "kind", ""},
		{"wrong type", map[string]any{"kind": 42}, "kind", ""},
		{"happy path", map[string]any{"kind": "session"}, "kind", "session"},
		{"empty string value", map[string]any{"kind": ""}, "kind", ""},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := metadataStringField(tc.m, tc.key); got != tc.want {
				t.Errorf("metadataStringField(%v, %q) = %q, want %q", tc.m, tc.key, got, tc.want)
			}
		})
	}
}

// TestDuplicateArrivalCheckQuery_SQLFragments is a structural (no-DB) test
// mirroring the existing callTranscriptDupCheckQuery test pattern in
// document_transcript_dedup_test.go: it pins the clauses that make the guard
// correct without requiring a live database.
func TestDuplicateArrivalCheckQuery_SQLFragments(t *testing.T) {
	t.Parallel()

	required := []struct {
		fragment string
		reason   string
	}{
		{"title       = $1", "must match on exact title"},
		{"occurred_at BETWEEN $2 AND $3", "must use an indexed range comparison on occurred_at"},
		{"source_type <> $4", "must exclude the incoming document's own source_type"},
		{"status       = 'active'", "must ignore soft-deleted/moved documents"},
		{"LIMIT 1", "must be a cheap existence probe, not a full match"},
	}

	for _, tc := range required {
		tc := tc
		t.Run(tc.reason, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(duplicateArrivalCheckQuery, tc.fragment) {
				t.Errorf("duplicateArrivalCheckQuery missing %q (%s)\nfull query:\n%s",
					tc.fragment, tc.reason, duplicateArrivalCheckQuery)
			}
		})
	}
}
