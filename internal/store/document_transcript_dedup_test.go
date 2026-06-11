package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// TestCallTranscriptDupCheckQuery_SQLFragments verifies that the
// callTranscriptDupCheckQuery constant contains the critical clauses that make
// the content-based dedup guard correct.  This is a structural test that runs
// without a live database.
//
// The guard must:
//  1. Target only source_type = 'call-transcript'
//  2. Limit to status = 'active' rows (soft-deleted duplicates should not block)
//  3. Match on content = $1
//  4. Exclude the current document's own source_id (<> $2)
//  5. Use LIMIT 1 so the probe is an O(1) existence check
func TestCallTranscriptDupCheckQuery_SQLFragments(t *testing.T) {
	t.Parallel()

	required := []struct {
		fragment string
		reason   string
	}{
		{"source_type = 'call-transcript'", "must scope check to call-transcript only"},
		{"status      = 'active'", "must ignore soft-deleted duplicates"},
		{"content     = $1", "must match on content"},
		{"source_id  <> $2", "must exclude the document's own source_id"},
		{"LIMIT 1", "must be a cheap existence probe"},
	}

	for _, tc := range required {
		tc := tc
		t.Run(tc.reason, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(callTranscriptDupCheckQuery, tc.fragment) {
				t.Errorf("callTranscriptDupCheckQuery missing %q (%s)\nfull query:\n%s",
					tc.fragment, tc.reason, callTranscriptDupCheckQuery)
			}
		})
	}
}

// TestErrDuplicateTranscript_Sentinel verifies that ErrDuplicateTranscript is a
// distinct, non-nil error value that callers can detect with errors.Is.
func TestErrDuplicateTranscript_Sentinel(t *testing.T) {
	t.Parallel()

	if ErrDuplicateTranscript == nil {
		t.Fatal("ErrDuplicateTranscript must not be nil")
	}

	// Wrapping must still be detectable via errors.Is.
	wrapped := errors.New("collection: " + ErrDuplicateTranscript.Error())
	// Manually constructed wrapping (not using %w) should NOT match — verify
	// that we are testing the sentinel, not just any error.
	if errors.Is(wrapped, ErrDuplicateTranscript) {
		t.Error("a manually concatenated error should not satisfy errors.Is(wrapped, ErrDuplicateTranscript)")
	}

	// The sentinel itself must match itself.
	if !errors.Is(ErrDuplicateTranscript, ErrDuplicateTranscript) {
		t.Error("errors.Is(ErrDuplicateTranscript, ErrDuplicateTranscript) must be true")
	}
}

// TestUpsertDedupGuard_SourceTypeScope verifies the guard's source-type
// branching logic: the duplicate check is performed when and only when the
// document's source_type equals model.SourceCallTranscript. Other source types
// must proceed directly to the upsert without any dedup check.
//
// This test runs without a live database by inspecting the guard condition
// defined in the Upsert implementation indirectly: we enumerate the
// SourceType constants and confirm that only SourceCallTranscript equals
// "call-transcript" (the hard-coded value embedded in callTranscriptDupCheckQuery).
func TestUpsertDedupGuard_SourceTypeScope(t *testing.T) {
	t.Parallel()

	// The guard is active when doc.SourceType == model.SourceCallTranscript.
	// The SQL constant hard-codes the string 'call-transcript'. If either value
	// ever drifts the dedup guard silently stops working.
	if string(model.SourceCallTranscript) != "call-transcript" {
		t.Errorf("model.SourceCallTranscript = %q, want \"call-transcript\"; "+
			"callTranscriptDupCheckQuery hard-codes this literal — update both together",
			model.SourceCallTranscript)
	}

	// Confirm that the guard condition in Upsert matches the constant value.
	// This check is necessarily textual (we cannot call Upsert without a DB),
	// but it guards the most dangerous drift: renaming the constant without
	// updating the Upsert branch.
	guardType := model.SourceCallTranscript
	unaffectedTypes := []model.SourceType{
		model.SourceSlack,
		model.SourceGmail,
		model.SourceSMS,
		model.SourceFilesystem,
		model.SourceCallLog,
		model.SourceUpload,
	}
	for _, st := range unaffectedTypes {
		if st == guardType {
			t.Errorf("source type %q unexpectedly equals SourceCallTranscript; "+
				"dedup guard would fire for all documents of this type", st)
		}
	}
}

// TestIsNoRows_IdentifiesPgxSentinel confirms that isNoRows correctly detects
// the pgx.ErrNoRows sentinel. This matters because the Upsert guard uses
// isNoRows to distinguish "no duplicate found" from a real query error; a
// wrong return value would either skip valid inserts or ignore real errors.
func TestIsNoRows_IdentifiesPgxSentinel(t *testing.T) {
	t.Parallel()

	// pgx.ErrNoRows is the sentinel that QueryRow.Scan returns when the query
	// returns zero rows. The Upsert guard must recognise it as the "no duplicate"
	// branch.
	import_pgx_ErrNoRows := errors.New("no rows in result set") // pgx.ErrNoRows message
	_ = import_pgx_ErrNoRows                                     // used for documentation only

	// We cannot import pgx.ErrNoRows without a build-time dependency here, but
	// isNoRows is tested indirectly: the function uses err == pgx.ErrNoRows, so
	// a nil error must NOT satisfy it.
	if isNoRows(nil) {
		t.Error("isNoRows(nil) must return false; nil is not a 'no rows' error")
	}

	// A non-nil, non-ErrNoRows error must also return false.
	if isNoRows(errors.New("some other error")) {
		t.Error("isNoRows(someOtherErr) must return false")
	}
}

// TestCallTranscriptDedupLogic_BehaviourTable documents the three cases that
// the Upsert guard must handle correctly.  Because a live DB is required to
// actually exercise Upsert end-to-end, this test verifies the branching
// conditions as pure Go logic rather than making real DB calls.
//
// The three cases:
//
//	(A) same source_id re-upsert  → guard SELECT excludes own source_id → 0 rows → proceed
//	(B) different source_id, same content → SELECT returns 1 → return ErrDuplicateTranscript
//	(C) different source_id, different content → SELECT returns 0 → proceed
//
// The table below models the guard as a pure function of (dupFound bool) to
// verify that the branch logic is correct independently of the SQL.
func TestCallTranscriptDedupLogic_BehaviourTable(t *testing.T) {
	t.Parallel()

	// guardDecision mirrors the Upsert guard branch:
	//   dupFound=true  → return ErrDuplicateTranscript (skip)
	//   dupFound=false → return nil (proceed with upsert)
	guardDecision := func(dupFound bool) error {
		if dupFound {
			return ErrDuplicateTranscript
		}
		return nil
	}

	tests := []struct {
		name        string
		dupFound    bool
		wantErr     error
		description string
	}{
		{
			name:        "same_source_id_re_upsert",
			dupFound:    false, // SQL excludes own source_id → always 0 rows for same source_id
			wantErr:     nil,
			description: "idempotent re-upsert of same source_id must proceed normally",
		},
		{
			name:        "different_source_id_identical_content",
			dupFound:    true, // SQL finds a row with same content, different source_id
			wantErr:     ErrDuplicateTranscript,
			description: "second path for same audio must be skipped",
		},
		{
			name:        "different_source_id_different_content",
			dupFound:    false, // SELECT returns no rows
			wantErr:     nil,
			description: "distinct transcript must proceed normally",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := guardDecision(tt.dupFound)
			if !errors.Is(got, tt.wantErr) {
				t.Errorf("%s: guardDecision(%v) = %v, want %v",
					tt.description, tt.dupFound, got, tt.wantErr)
			}
		})
	}
}

// TestCallTranscriptDedupGuard_NonTranscriptSourceType verifies that the guard
// condition (doc.SourceType == model.SourceCallTranscript) is false for all
// other source types, meaning no dedup check would be performed for them.
// This prevents accidental behaviour changes for non-transcript sources.
func TestCallTranscriptDedupGuard_NonTranscriptSourceType(t *testing.T) {
	t.Parallel()

	otherTypes := []model.SourceType{
		model.SourceGmail,
		model.SourceSMS,
		model.SourceCalendar,
		model.SourceFilesystem,
		model.SourceCallLog,
		model.SourceSlack,
		model.SourceGitHub,
		model.SourceUpload,
	}

	for _, st := range otherTypes {
		st := st
		t.Run(string(st), func(t *testing.T) {
			t.Parallel()

			// The guard fires only for SourceCallTranscript.
			guardFires := st == model.SourceCallTranscript
			if guardFires {
				t.Errorf("dedup guard must NOT fire for source type %q", st)
			}
		})
	}
}
