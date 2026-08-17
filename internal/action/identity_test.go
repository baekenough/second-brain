package action

import (
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

func TestNormalizeSummary_AwaitingMyReply_AlwaysEmpty(t *testing.T) {
	t.Parallel()
	tests := []string{"", "  ", "Please respond soon!", "완전히 다른 문장입니다."}
	for _, s := range tests {
		if got := NormalizeSummary(string(model.KindAwaitingMyReply), s); got != "" {
			t.Errorf("NormalizeSummary(awaiting_my_reply, %q) = %q, want empty", s, got)
		}
	}
}

func TestNormalizeSummary_OtherKinds_FoldsWhitespaceCaseAndPunctuation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b string
	}{
		{"case", "Send the report.", "send the report."},
		{"whitespace", "send   the  report.", "send the report."},
		{"punctuation", "send the report!!", "send the report"},
		{"trim", "  send the report.  ", "send the report."},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			na := NormalizeSummary(string(model.KindMyCommitment), tc.a)
			nb := NormalizeSummary(string(model.KindMyCommitment), tc.b)
			if na != nb {
				t.Errorf("normalization did not converge: %q vs %q -> %q vs %q", tc.a, tc.b, na, nb)
			}
		})
	}
}

func TestNormalizeSummary_OtherKinds_TruncatesAt80Chars(t *testing.T) {
	t.Parallel()
	long := ""
	for i := 0; i < 200; i++ {
		long += "a"
	}
	got := NormalizeSummary(string(model.KindMyCommitment), long)
	if len(got) != 80 {
		t.Errorf("len(NormalizeSummary(...)) = %d, want 80", len(got))
	}
}

// TestBuildIdentityKey_AwaitingMyReply_StructuralAndLLMConverge is the load-
// bearing test for spec §7.2's merge requirement: a structural candidate
// (fixed template summary) and an LLM candidate (free-text summary) for the
// SAME thread/kind/counterpart MUST produce the same identity_key, or
// detected_by can never become "both".
func TestBuildIdentityKey_AwaitingMyReply_StructuralAndLLMConverge(t *testing.T) {
	t.Parallel()
	structural := BuildIdentityKey("gmail:t1", string(model.KindAwaitingMyReply), "alice@example.com",
		"마지막 메시지 이후 응답 없음")
	llm := BuildIdentityKey("gmail:t1", string(model.KindAwaitingMyReply), "alice@example.com",
		"Alice is waiting on a reply from you regarding the Q3 budget.")
	if structural != llm {
		t.Errorf("structural key %q != llm key %q — awaiting_my_reply must ignore summary text", structural, llm)
	}
}

func TestBuildIdentityKey_OtherKinds_DifferentSummary_DifferentKey(t *testing.T) {
	t.Parallel()
	a := BuildIdentityKey("gmail:t1", string(model.KindMyCommitment), "alice@example.com", "Send the Q3 report by Friday")
	b := BuildIdentityKey("gmail:t1", string(model.KindMyCommitment), "alice@example.com", "Call Bob about the contract")
	if a == b {
		t.Error("two distinct commitments in the same thread collapsed to one identity_key")
	}
}

func TestBuildIdentityKey_Deterministic(t *testing.T) {
	t.Parallel()
	a := BuildIdentityKey("sms:철수", string(model.KindMyCommitment), "철수", "밥 사주기로 함")
	b := BuildIdentityKey("sms:철수", string(model.KindMyCommitment), "철수", "밥 사주기로 함")
	if a != b {
		t.Error("BuildIdentityKey is not deterministic for identical inputs")
	}
}

func TestBuildIdentityKey_FormatMatchesActionPrefixAnd16HexChars(t *testing.T) {
	t.Parallel()
	k := BuildIdentityKey("sms:철수", string(model.KindMyCommitment), "철수", "밥 사주기로 함")
	if len(k) != len("action:")+16 {
		t.Errorf("identity_key length = %d, want %d (action: + 16 hex chars)", len(k), len("action:")+16)
	}
	if k[:7] != "action:" {
		t.Errorf("identity_key prefix = %q, want \"action:\"", k[:7])
	}
}

// TestCounterpartIdentity_GmailFromUser_NotAwaitingReply pins the direction
// inference rule (spec §7.1): a gmail message the user themself sent cannot
// generate an awaiting_my_reply signal.
func TestCounterpartIdentity_GmailFromUser_NotAwaitingReply(t *testing.T) {
	t.Parallel()
	doc := &model.Document{
		SourceType: model.SourceGmail,
		Metadata:   map[string]any{"from": "me@example.com"},
	}
	isUser := func(c string) bool { return c == "me@example.com" }
	_, ok := CounterpartIdentity(doc, isUser)
	if ok {
		t.Error("CounterpartIdentity should return ok=false when from is the user's own address")
	}
}

func TestCounterpartIdentity_GmailFromOther_ReturnsFromAddress(t *testing.T) {
	t.Parallel()
	doc := &model.Document{
		SourceType: model.SourceGmail,
		Metadata:   map[string]any{"from": "alice@example.com"},
	}
	isUser := func(c string) bool { return c == "me@example.com" }
	identity, ok := CounterpartIdentity(doc, isUser)
	if !ok || identity != "alice@example.com" {
		t.Errorf("CounterpartIdentity = (%q, %v), want (alice@example.com, true)", identity, ok)
	}
}

func TestCounterpartIdentity_SMSContactName(t *testing.T) {
	t.Parallel()
	doc := &model.Document{
		SourceType: model.SourceSMS,
		Metadata:   map[string]any{"contact_name": "철수"},
	}
	identity, ok := CounterpartIdentity(doc, func(string) bool { return false })
	if !ok || identity != "철수" {
		t.Errorf("CounterpartIdentity = (%q, %v), want (철수, true)", identity, ok)
	}
}

func TestCounterpartIdentity_SMSNoContactName_FallsBackToHashedNumber(t *testing.T) {
	t.Parallel()
	doc := &model.Document{
		SourceType: model.SourceSMS,
		Metadata:   map[string]any{"number": "010-1234-5678"},
	}
	identity, ok := CounterpartIdentity(doc, func(string) bool { return false })
	if !ok || identity == "" || identity == "010-1234-5678" {
		t.Errorf("CounterpartIdentity = (%q, %v), want a hashed non-empty identity", identity, ok)
	}
	if identity != HashPhoneNumber("010-1234-5678") {
		t.Errorf("CounterpartIdentity(sms, no contact_name) = %q, want HashPhoneNumber(number)", identity)
	}
}

func TestCounterpartIdentity_UnsupportedSourceType(t *testing.T) {
	t.Parallel()
	doc := &model.Document{SourceType: model.SourceSlack}
	_, ok := CounterpartIdentity(doc, func(string) bool { return false })
	if ok {
		t.Error("CounterpartIdentity should return ok=false for unsupported source types")
	}
}

func TestIsUserAddress(t *testing.T) {
	t.Parallel()
	addrs := []string{"me@example.com", " Alias@Example.com "}
	tests := []struct {
		candidate string
		want      bool
	}{
		{"me@example.com", true},
		{"ME@EXAMPLE.COM", true},
		{"alias@example.com", true},
		{"", false},
		{"other@example.com", false},
	}
	for _, tc := range tests {
		if got := IsUserAddress(addrs, tc.candidate); got != tc.want {
			t.Errorf("IsUserAddress(%v, %q) = %v, want %v", addrs, tc.candidate, got, tc.want)
		}
	}
}

func TestThreadKeyBuilders(t *testing.T) {
	t.Parallel()
	if got := ThreadKeyGmail("t1"); got != "gmail:t1" {
		t.Errorf("ThreadKeyGmail = %q", got)
	}
	if got := ThreadKeySMS("철수"); got != "sms:철수" {
		t.Errorf("ThreadKeySMS = %q", got)
	}
	if got := ThreadKeyCall("철수"); got != "call:철수" {
		t.Errorf("ThreadKeyCall = %q", got)
	}
}

func TestHashPhoneNumber_DeterministicAnd16HexChars(t *testing.T) {
	t.Parallel()
	a := HashPhoneNumber("010-1234-5678")
	b := HashPhoneNumber("010-1234-5678")
	if a != b {
		t.Error("HashPhoneNumber is not deterministic")
	}
	if len(a) != 16 {
		t.Errorf("len(HashPhoneNumber(...)) = %d, want 16", len(a))
	}
}
