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

func TestListLegacyForRecheckQuery_ExcludesOwnAndUserClassifiers(t *testing.T) {
	if !strings.Contains(listLegacyForRecheckQuery, "metadata->>'classifier' NOT IN ('rule', 'jev-latest', 'user')") {
		t.Errorf("listLegacyForRecheckQuery does not exclude rule/jev-latest/user classifiers:\n%s", listLegacyForRecheckQuery)
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
