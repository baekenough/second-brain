package store

import (
	"strings"
	"testing"
)

// These tests pin the shape of the classification worker's SQL without a
// live database — the same "assert on the query text" convention used
// throughout this package (see document_retention_test.go). They exist
// because the classifier="user" golden-set protection and the
// classifier_gate_checked_at anti-starvation marker are both load-bearing
// SQL predicates that a future refactor could silently drop.

func TestListUnclassifiedQuery_ScopesToActiveClassifiableSources(t *testing.T) {
	for _, want := range []string{
		"status = 'active'",
		"source_type IN ('sms', 'gmail', 'call-transcript')",
		"NOT (metadata ? 'retention')",
	} {
		if !strings.Contains(listUnclassifiedQuery, want) {
			t.Errorf("listUnclassifiedQuery missing %q:\n%s", want, listUnclassifiedQuery)
		}
	}
}

func TestListUnclassifiedQuery_RespectsAttemptCapAndBackfillWindow(t *testing.T) {
	if !strings.Contains(listUnclassifiedQuery, "classifier_attempts')::int, 0) < 3") {
		t.Errorf("listUnclassifiedQuery does not cap retries at 3 attempts:\n%s", listUnclassifiedQuery)
	}
	if !strings.Contains(listUnclassifiedQuery, "$2::int <= 0 OR collected_at >= now() - make_interval(days => $2::int)") {
		t.Errorf("listUnclassifiedQuery does not implement the backfillDays<=0-means-unbounded contract:\n%s", listUnclassifiedQuery)
	}
	if !strings.Contains(listUnclassifiedQuery, "ORDER BY collected_at DESC") {
		t.Errorf("listUnclassifiedQuery must order newest-first (spec):\n%s", listUnclassifiedQuery)
	}
}

// TestListLegacyForRecheckQuery_ScopesToRetentionTaggedNonUserRows pins the
// fix for the "worker sat idle since 12:57" bug: the queue must select on
// `metadata ? 'retention'` (any retention-tagged row), not a bare
// `metadata ? 'classifier'` requirement — the ox-alpha mail segmentation
// pass tags 11,965 legacy gmail documents with retention/segment but never
// writes a classifier key at all, so the old predicate silently excluded
// all of them, leaving the queue permanently empty. The classifier
// exclusion is narrowed to "not user" (golden-set) rather than
// "not rule/jev-latest/user" — a NOT IN list would also permanently skip
// the 3,387 sms + 4,050 call-transcript documents an old backfill script
// tagged classifier="jev-latest"/"rule" without ever running them through
// the deterministic Gate.
func TestListLegacyForRecheckQuery_ScopesToRetentionTaggedNonUserRows(t *testing.T) {
	for _, want := range []string{
		"metadata ? 'retention'",
		"COALESCE(metadata->>'classifier', '') <> 'user'",
	} {
		if !strings.Contains(listLegacyForRecheckQuery, want) {
			t.Errorf("listLegacyForRecheckQuery missing %q:\n%s", want, listLegacyForRecheckQuery)
		}
	}
	if strings.Contains(listLegacyForRecheckQuery, "metadata ? 'classifier'") {
		t.Errorf("listLegacyForRecheckQuery must not require a bare 'classifier' key — that excludes ox-alpha-tagged docs which only set retention/segment:\n%s", listLegacyForRecheckQuery)
	}
	if strings.Contains(listLegacyForRecheckQuery, "NOT IN ('rule', 'jev-latest', 'user')") {
		t.Errorf("listLegacyForRecheckQuery must not blanket-exclude rule/jev-latest classifiers — legacy backfill-tagged docs with those values may still be un-audited:\n%s", listLegacyForRecheckQuery)
	}
}

func TestListLegacyForRecheckQuery_SkipsAlreadyGateCheckedRows(t *testing.T) {
	if !strings.Contains(listLegacyForRecheckQuery, "NOT (metadata ? 'classifier_gate_checked_at')") {
		t.Errorf("listLegacyForRecheckQuery does not skip already-audited rows (would starve the backlog):\n%s", listLegacyForRecheckQuery)
	}
	if !strings.Contains(listLegacyForRecheckQuery, "ORDER BY collected_at ASC") {
		t.Errorf("listLegacyForRecheckQuery must make forward progress oldest-first:\n%s", listLegacyForRecheckQuery)
	}
}

// TestMergeClassificationMetadataQuery_NeverOverwritesUserClassifier is the
// SQL-level half of the spec's "classifier=\"user\" 문서는 어떤 모드에서도
// 덮어쓰지 않음" guarantee. See internal/classify's
// TestResult_Metadata_NeverEmitsUserClassifier for the application-level
// half (this package never produces that value itself).
func TestMergeClassificationMetadataQuery_NeverOverwritesUserClassifier(t *testing.T) {
	if !strings.Contains(mergeClassificationMetadataQuery, "COALESCE(metadata->>'classifier', '') <> 'user'") {
		t.Fatalf("mergeClassificationMetadataQuery does not guard against overwriting classifier=\"user\":\n%s", mergeClassificationMetadataQuery)
	}
	if !strings.Contains(mergeClassificationMetadataQuery, "metadata = metadata ||") {
		t.Errorf("mergeClassificationMetadataQuery does not merge (preserve existing keys):\n%s", mergeClassificationMetadataQuery)
	}
}
