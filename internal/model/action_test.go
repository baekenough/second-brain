package model

import "testing"

func TestIsValidActionKind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		kind ActionKind
		want bool
	}{
		{"awaiting_my_reply valid", KindAwaitingMyReply, true},
		{"my_commitment valid", KindMyCommitment, true},
		{"their_commitment valid", KindTheirCommitment, true},
		{"scheduled valid", KindScheduled, true},
		{"invented kind invalid", ActionKind("follow_up"), false},
		{"empty kind invalid", ActionKind(""), false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsValidActionKind(tc.kind); got != tc.want {
				t.Errorf("IsValidActionKind(%q) = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}

// TestActionKindDetectedByState_WireValues pins the exact string values
// (spec §5.4 table) that internal/store/document.go's SQL literals and
// internal/action's identity_key hash both depend on — a silent rename here
// would desync them without a compile error.
func TestActionKindDetectedByState_WireValues(t *testing.T) {
	t.Parallel()
	if string(KindAwaitingMyReply) != "awaiting_my_reply" {
		t.Errorf("KindAwaitingMyReply = %q", KindAwaitingMyReply)
	}
	if string(KindMyCommitment) != "my_commitment" {
		t.Errorf("KindMyCommitment = %q", KindMyCommitment)
	}
	if string(KindTheirCommitment) != "their_commitment" {
		t.Errorf("KindTheirCommitment = %q", KindTheirCommitment)
	}
	if string(KindScheduled) != "scheduled" {
		t.Errorf("KindScheduled = %q", KindScheduled)
	}
	if string(DetectedStructural) != "structural" || string(DetectedLLM) != "llm" || string(DetectedBoth) != "both" {
		t.Error("DetectedBy wire values drifted from spec §5.4")
	}
	if string(StateOpen) != "open" || string(StateDone) != "done" || string(StateIgnored) != "ignored" {
		t.Error("ActionState wire values drifted from spec §5.4")
	}
}
