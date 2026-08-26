package model

import "testing"

// TestKnownSourceTypes_MatchesTaskSpec pins the exact corpus-based known-source
// list. A silent addition/removal here changes what internal/store's
// checkSourceTypeGuard treats as "known" without a code review signal beyond
// this test failing.
func TestKnownSourceTypes_MatchesTaskSpec(t *testing.T) {
	t.Parallel()

	want := map[SourceType]bool{
		SourceGmail:          true,
		SourceSMS:            true,
		SourceCallLog:        true,
		SourceCallTranscript: true,
		SourceCalendar:       true,
		SourceInsight:        true,
		SourceNote:           true,
		SourceUpload:         true,
		SourceAgentNote:      true,
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

	for _, st := range []SourceType{SourceGmail, SourceSMS, SourceAgentNote, SourceUpload} {
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
	for _, st := range []SourceType{SourceGmail, SourceSMS, SourceLLMMemory, SourceAgentNote} {
		if IsContainerSourceType(st) {
			t.Errorf("IsContainerSourceType(%q) = true, want false", st)
		}
	}
}

// TestIsDeprecatedSourceType verifies only llm-memory is flagged as
// deprecated, and that the newly introduced agent-note replacement is NOT
// itself flagged (a regression here would make every add_note call warn).
func TestIsDeprecatedSourceType(t *testing.T) {
	t.Parallel()

	if !IsDeprecatedSourceType(SourceLLMMemory) {
		t.Error("IsDeprecatedSourceType(llm-memory) = false, want true")
	}
	for _, st := range []SourceType{SourceAgentNote, SourceNote, SourceGmail, SourceSecretary} {
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
