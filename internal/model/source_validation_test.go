package model

import "testing"

// TestKnownSourceTypes_MatchesTaskSpec pins the exact corpus-based known-source
// list. A silent addition/removal here changes what internal/store's
// checkSourceTypeGuard treats as "known" without a code review signal beyond
// this test failing.
func TestKnownSourceTypes_MatchesTaskSpec(t *testing.T) {
	t.Parallel()

	want := map[SourceType]bool{
		SourceSMS:       true,
		SourceCall:      true,
		SourceInsight:   true,
		SourceNote:      true,
		SourceUpload:    true,
		SourceAgentNote: true,
	}

	got := KnownSourceTypes()
	if len(got) != len(want) {
		t.Fatalf("KnownSourceTypes() has %d entries, want %d: %v", len(got), len(want), got)
	}
	seen := map[SourceType]bool{}
	for _, st := range got {
		if !want[st] {
			t.Errorf("KnownSourceTypes() contains unexpected %q", st)
		}
		if seen[st] {
			t.Errorf("KnownSourceTypes() contains duplicate %q", st)
		}
		seen[st] = true
	}
}

// TestIsKnownSourceType_ExcludesContainerAndDeprecated verifies the guard's
// two most important negatives: the container type (secretary) and the
// deprecated type (llm-memory) must NOT be reported as known, or
// checkSourceTypeGuard's switch would never reach their dedicated branches.
func TestIsKnownSourceType_ExcludesContainerAndDeprecated(t *testing.T) {
	t.Parallel()

	for _, st := range []SourceType{SourceSecretary, SourceLLMMemory} {
		if IsKnownSourceType(st) {
			t.Errorf("IsKnownSourceType(%q) = true, want false", st)
		}
	}
}

// TestIsKnownSourceType_Positive spot-checks a few known types.
func TestIsKnownSourceType_Positive(t *testing.T) {
	t.Parallel()

	for _, st := range []SourceType{SourceSMS, SourceCall, SourceAgentNote, SourceUpload} {
		if !IsKnownSourceType(st) {
			t.Errorf("IsKnownSourceType(%q) = false, want true", st)
		}
	}
}

// TestIsContainerSourceType verifies only secretary is flagged as a
// container type — a false positive here would make the guard warn on
// legitimate per-kind writes.
func TestIsContainerSourceType(t *testing.T) {
	t.Parallel()

	if !IsContainerSourceType(SourceSecretary) {
		t.Error("IsContainerSourceType(secretary) = false, want true")
	}
	for _, st := range []SourceType{SourceCall, SourceSMS, SourceLLMMemory, SourceAgentNote} {
		if IsContainerSourceType(st) {
			t.Errorf("IsContainerSourceType(%q) = true, want false", st)
		}
	}
}

// TestIsDeprecatedSourceType verifies llm-memory and the two legacy call
// types (call-log, call-transcript — unified into SourceCall by migration
// 033, see that const's doc comment) are flagged as deprecated, and that
// their live replacements (agent-note, call) are NOT themselves flagged (a
// regression here would make every add_note / call ingest warn).
func TestIsDeprecatedSourceType(t *testing.T) {
	t.Parallel()

	for _, st := range []SourceType{SourceLLMMemory, SourceCallLog, SourceCallTranscript} {
		if !IsDeprecatedSourceType(st) {
			t.Errorf("IsDeprecatedSourceType(%q) = false, want true", st)
		}
	}
	for _, st := range []SourceType{SourceAgentNote, SourceCall, SourceNote, SourceSMS, SourceSecretary} {
		if IsDeprecatedSourceType(st) {
			t.Errorf("IsDeprecatedSourceType(%q) = true, want false", st)
		}
	}
}

// TestContainerAndDeprecatedSourceTypes_Disjoint verifies the two guard
// categories never overlap — checkSourceTypeGuard's switch relies on
// container being checked before deprecated, and an overlapping value would
// make the ordering silently matter in a way this test would catch first.
func TestContainerAndDeprecatedSourceTypes_Disjoint(t *testing.T) {
	t.Parallel()

	for _, c := range ContainerSourceTypes() {
		for _, d := range DeprecatedSourceTypes() {
			if c == d {
				t.Errorf("%q is both a container type and a deprecated type", c)
			}
		}
	}
}

// TestSourceAgentNote_StringValue pins the wire-format string value, since
// cmd/mcp/main.go's tool description and error strings hard-code "agent-note".
func TestSourceAgentNote_StringValue(t *testing.T) {
	t.Parallel()
	if string(SourceAgentNote) != "agent-note" {
		t.Errorf("SourceAgentNote = %q, want %q", SourceAgentNote, "agent-note")
	}
}

// TestNormalizeSourceType_LegacyCallAliases verifies both pre-migration-033
// values resolve to the value migration 033 unified them into. This is the
// exact bug this function exists to fix: a filter still spelled with either
// old value matched zero rows post-migration (see NormalizeSourceType's doc
// comment) with no error signal distinguishing it from "no such documents".
func TestNormalizeSourceType_LegacyCallAliases(t *testing.T) {
	t.Parallel()

	for _, st := range []SourceType{SourceCallLog, SourceCallTranscript} {
		if got := NormalizeSourceType(st); got != SourceCall {
			t.Errorf("NormalizeSourceType(%q) = %q, want %q", st, got, SourceCall)
		}
	}
}

// TestNormalizeSourceType_NonAliasesPassThrough verifies every other value —
// current source types AND the one deprecated value with no replacement
// (SourceLLMMemory, decommissioned rather than merged) — returns unchanged.
// A regression that widened the alias map to match unrelated values would
// silently rewrite a caller's request to a different source type.
func TestNormalizeSourceType_NonAliasesPassThrough(t *testing.T) {
	t.Parallel()

	for _, st := range []SourceType{
		SourceCall, SourceSMS, SourceNote,
		SourceUpload, SourceAgentNote, SourceInsight, SourceLLMMemory,
		SourceSecretary, SourceType("unknown-source"),
	} {
		if got := NormalizeSourceType(st); got != st {
			t.Errorf("NormalizeSourceType(%q) = %q, want unchanged %q", st, got, st)
		}
	}
}

// TestNormalizeSourceType_Idempotent verifies re-normalizing an already
// normalized value is a no-op — several callers (SearchQuery.IncludeSourceTypes
// plus internal/intent/plan.go's parseSourceTypes) may both normalize the same
// value on one request, and a non-idempotent mapping would make that layering
// order-sensitive.
func TestNormalizeSourceType_Idempotent(t *testing.T) {
	t.Parallel()

	for _, st := range []SourceType{SourceCallLog, SourceCallTranscript, SourceCall, SourceSMS} {
		once := NormalizeSourceType(st)
		twice := NormalizeSourceType(once)
		if once != twice {
			t.Errorf("NormalizeSourceType(%q) = %q, but NormalizeSourceType(that) = %q, want idempotent", st, once, twice)
		}
	}
}

// TestRemovedSourceTypes_AreUnknown 은 gmail·calendar 가 더는 소스로 인정되지
// 않음을 고정한다. 수집기가 제거됐으므로 이 값을 이름으로 든 요청이나 남아 있는
// 행은 알 수 없는 다른 source_type 과 똑같이 취급된다(알려진 값도, 폐기 값도,
// 컨테이너도, 별칭도 아니다).
func TestRemovedSourceTypes_AreUnknown(t *testing.T) {
	t.Parallel()

	for _, st := range []SourceType{"gmail", "calendar"} {
		if IsKnownSourceType(st) {
			t.Errorf("IsKnownSourceType(%q) = true, want false", st)
		}
		if IsDeprecatedSourceType(st) || IsContainerSourceType(st) {
			t.Errorf("%q must not be classified as deprecated or container", st)
		}
		if got := NormalizeSourceType(st); got != st {
			t.Errorf("NormalizeSourceType(%q) = %q, want unchanged", st, got)
		}
	}
}
