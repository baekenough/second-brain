package intent_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/llm"
)

// fakeCompleter is a minimal llm.Completer test double. It never fails
// silently: err (if set) is always returned, and every call is counted so
// tests can assert the LLM fallback was (or was not) invoked.
type fakeCompleter struct {
	calls    int
	response string
	err      error
}

func (f *fakeCompleter) Enabled() bool { return true }

func (f *fakeCompleter) CompleteWithMessages(_ context.Context, _ string, _ []llm.Message) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.response, nil
}

func newClassifier(t *testing.T, c llm.Completer, now time.Time) *intent.LLMClassifier {
	t.Helper()
	cl := intent.NewLLMClassifier(c)
	intent.SetNow(cl, func() time.Time { return now })
	return cl
}

// fixedNow anchors all relative date-phrase tests to a stable reference
// time (2026-08-17, a Monday) so results don't depend on the real clock.
var fixedNow = time.Date(2026, 8, 17, 15, 0, 0, 0, time.UTC)

func TestLLMClassifier_Classify_DeterministicDatePhrases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		question   string
		wantFrom   time.Time
		wantTo     time.Time
		wantConfid float64
	}{
		{
			name:       "explicit year and month",
			question:   "2026년 6월 요약해줘",
			wantFrom:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			wantTo:     time.Date(2026, 6, 30, 23, 59, 59, 0, time.UTC),
			wantConfid: 1.0,
		},
		{
			name:       "last month, relative to fixed now (2026-08-17)",
			question:   "지난달에 뭐 있었지",
			wantFrom:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			wantTo:     time.Date(2026, 7, 31, 23, 59, 59, 0, time.UTC),
			wantConfid: 1.0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Fails if the LLM fallback is ever invoked for a deterministic match.
			fake := &fakeCompleter{err: errors.New("LLM must not be called for a deterministic date match")}
			c := newClassifier(t, fake, fixedNow)

			got, err := c.Classify(context.Background(), tc.question)
			if err != nil {
				t.Fatalf("Classify returned non-nil error (contract violation): %v", err)
			}
			if got.Kind != intent.KindTemporal {
				t.Fatalf("Kind: want %q, got %q", intent.KindTemporal, got.Kind)
			}
			if got.Confidence != tc.wantConfid {
				t.Fatalf("Confidence: want %v, got %v", tc.wantConfid, got.Confidence)
			}
			if got.OccurredFrom == nil || !got.OccurredFrom.Equal(tc.wantFrom) {
				t.Fatalf("OccurredFrom: want %v, got %v", tc.wantFrom, got.OccurredFrom)
			}
			if got.OccurredTo == nil || !got.OccurredTo.Equal(tc.wantTo) {
				t.Fatalf("OccurredTo: want %v, got %v", tc.wantTo, got.OccurredTo)
			}
			if got.RawQuery != tc.question {
				t.Fatalf("RawQuery: want %q, got %q", tc.question, got.RawQuery)
			}
			if fake.calls != 0 {
				t.Fatalf("expected LLM fallback never called, got %d calls", fake.calls)
			}
		})
	}
}

func TestLLMClassifier_Classify_PhoneNumberIsExactToken(t *testing.T) {
	t.Parallel()

	fake := &fakeCompleter{err: errors.New("LLM must not be called for a deterministic phone match")}
	c := newClassifier(t, fake, fixedNow)

	got, err := c.Classify(context.Background(), "010-1234-5678로 전화해줘")
	if err != nil {
		t.Fatalf("Classify returned non-nil error: %v", err)
	}
	if got.Kind != intent.KindExactToken {
		t.Fatalf("Kind: want %q, got %q", intent.KindExactToken, got.Kind)
	}
	if got.Confidence != 1.0 {
		t.Fatalf("Confidence: want 1.0, got %v", got.Confidence)
	}
	if fake.calls != 0 {
		t.Fatalf("expected LLM fallback never called, got %d calls", fake.calls)
	}
}

// TestLLMClassifier_Classify_TemporalPrecedence confirms the deterministic
// pre-pass short-circuits the LLM fallback entirely for a temporal match —
// a distinct assertion from the date-phrase value tests above, focused
// purely on "was the LLM invoked at all".
func TestLLMClassifier_Classify_TemporalPrecedence(t *testing.T) {
	t.Parallel()

	fake := &fakeCompleter{response: `{"kind":"entity","entity_name":"should not be used","confidence":0.9}`}
	c := newClassifier(t, fake, fixedNow)

	got, err := c.Classify(context.Background(), "오늘 있었던 일 알려줘")
	if err != nil {
		t.Fatalf("Classify returned non-nil error: %v", err)
	}
	if got.Kind != intent.KindTemporal {
		t.Fatalf("Kind: want %q, got %q", intent.KindTemporal, got.Kind)
	}
	if fake.calls != 0 {
		t.Fatalf("deterministic date match must short-circuit the LLM call, got %d calls", fake.calls)
	}
}

func TestLLMClassifier_Classify_LLMFallback_ValidEntity(t *testing.T) {
	t.Parallel()

	fake := &fakeCompleter{response: `{"kind":"entity","entity_name":"김대표","confidence":0.87}`}
	c := newClassifier(t, fake, fixedNow)

	got, err := c.Classify(context.Background(), "김대표랑 나눈 얘기 요약해줘")
	if err != nil {
		t.Fatalf("Classify returned non-nil error: %v", err)
	}
	if got.Kind != intent.KindEntity {
		t.Fatalf("Kind: want %q, got %q", intent.KindEntity, got.Kind)
	}
	if got.EntityName != "김대표" {
		t.Fatalf("EntityName: want %q, got %q", "김대표", got.EntityName)
	}
	if got.Confidence != 0.87 {
		t.Fatalf("Confidence: want 0.87, got %v", got.Confidence)
	}
	if fake.calls != 1 {
		t.Fatalf("expected exactly 1 LLM call, got %d", fake.calls)
	}
}

func TestLLMClassifier_Classify_LLMFallback_MalformedJSONDegradesToGeneral(t *testing.T) {
	t.Parallel()

	fake := &fakeCompleter{response: `this is not json`}
	c := newClassifier(t, fake, fixedNow)

	got, err := c.Classify(context.Background(), "그거 어떻게 됐어")
	if err != nil {
		t.Fatalf("Classify returned non-nil error (contract violation): %v", err)
	}
	if got.Kind != intent.KindGeneral {
		t.Fatalf("Kind: want %q, got %q", intent.KindGeneral, got.Kind)
	}
	if got.Confidence != 0 {
		t.Fatalf("Confidence: want 0, got %v", got.Confidence)
	}
	if fake.calls != 1 {
		t.Fatalf("expected exactly 1 LLM call, got %d", fake.calls)
	}
}

func TestLLMClassifier_Classify_LLMFallback_ErrorDegradesToGeneral(t *testing.T) {
	t.Parallel()

	fake := &fakeCompleter{err: errors.New("llm: upstream unavailable")}
	c := newClassifier(t, fake, fixedNow)

	got, err := c.Classify(context.Background(), "그거 어떻게 됐어")
	if err != nil {
		t.Fatalf("Classify returned non-nil error (contract violation): %v", err)
	}
	if got.Kind != intent.KindGeneral {
		t.Fatalf("Kind: want %q, got %q", intent.KindGeneral, got.Kind)
	}
	if got.Confidence != 0 {
		t.Fatalf("Confidence: want 0, got %v", got.Confidence)
	}
	if fake.calls != 1 {
		t.Fatalf("expected exactly 1 LLM call, got %d", fake.calls)
	}
}

// Compile-time assertion: *LLMClassifier satisfies Classifier.
var _ intent.Classifier = (*intent.LLMClassifier)(nil)
