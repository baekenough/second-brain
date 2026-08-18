package briefing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/llm"
)

// stubCompleter adapts a function to llm.Completer and records what it was
// asked. The prompt is captured so tests can assert on what the model is shown;
// it is never logged by production code.
type stubCompleter struct {
	enabled    bool
	calls      int
	gotSystem  string
	gotMessage string
	reply      string
	err        error
}

func (s *stubCompleter) Enabled() bool { return s.enabled }

func (s *stubCompleter) CompleteWithMessages(_ context.Context, system string, msgs []llm.Message) (string, error) {
	s.calls++
	s.gotSystem = system
	if len(msgs) > 0 {
		s.gotMessage = msgs[len(msgs)-1].Content
	}
	return s.reply, s.err
}

// TestGenerateReturnsGroundedSentences pins the happy path end to end.
func TestGenerateReturnsGroundedSentences(t *testing.T) {
	c := &stubCompleter{
		enabled: true,
		reply:   `{"sentences":[{"text":"근거 있는 문장.","document_ids":["` + docA + `"],"identity_keys":["` + keyA + `"]}]}`,
	}

	got, err := Generate(context.Background(), c, testInput())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got.Degraded || len(got.Sentences) != 1 {
		t.Fatalf("result = %+v, want one grounded sentence", got)
	}
	if got.GeneratedAt.IsZero() {
		t.Error("GeneratedAt is zero")
	}
}

// TestGenerateFallsBackOnLLMFailure pins that no LLM failure mode can turn into
// an error for the caller: a briefing is a convenience surface, and failing it
// must not take the actions page down with it.
func TestGenerateFallsBackOnLLMFailure(t *testing.T) {
	cases := map[string]error{
		"transport error":  errors.New("dummy transport failure"),
		"context canceled": context.Canceled,
		// A response cut off by the token budget is the dangerous case: its JSON
		// is syntactically incomplete, and treating a partial answer as a whole
		// one is exactly how an unfounded sentence reaches the user.
		"truncated by token budget": fmt.Errorf("wrapped: %w", llm.ErrTruncated),
	}
	for name, injected := range cases {
		t.Run(name, func(t *testing.T) {
			c := &stubCompleter{enabled: true, err: injected, reply: `{"sentences":[{"text":"잘린 문장`}

			got, err := Generate(context.Background(), c, testInput())
			if err != nil {
				t.Fatalf("Generate returned an error instead of degrading: %v", err)
			}
			if !got.Degraded {
				t.Fatal("Degraded = false after an LLM failure")
			}
			if len(got.Sentences) != 1 || len(got.Sentences[0].DocumentIDs) == 0 {
				t.Fatalf("fallback = %+v, want one grounded count sentence", got.Sentences)
			}
			if strings.Contains(got.Sentences[0].Text, "잘린 문장") {
				t.Fatal("partial model output leaked into the result")
			}
		})
	}
}

// TestGenerateSkipsDisabledClient pins that a server with no LLM configured
// returns the deterministic aggregate instead of calling out.
func TestGenerateSkipsDisabledClient(t *testing.T) {
	c := &stubCompleter{enabled: false, reply: `{"sentences":[]}`}

	got, err := Generate(context.Background(), c, testInput())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if c.calls != 0 {
		t.Fatalf("disabled client was called %d times", c.calls)
	}
	if !got.Degraded {
		t.Fatal("Degraded = false with no LLM available")
	}
}

// TestGenerateWithNoActions pins that an empty open set never reaches the model.
// Asking for a summary of nothing is a guaranteed hallucination.
func TestGenerateWithNoActions(t *testing.T) {
	c := &stubCompleter{enabled: true, reply: `{"sentences":[{"text":"없는 일을 지어냄.","document_ids":["` + docA + `"]}]}`}

	got, err := Generate(context.Background(), c, Input{Model: "dummy-model"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if c.calls != 0 {
		t.Fatalf("LLM called %d times for an empty action set", c.calls)
	}
	if len(got.Sentences) != 0 || got.Degraded {
		t.Fatalf("result = %+v, want an empty non-degraded briefing", got)
	}
}

// TestPromptCarriesWhitelistAndRule pins the two things the prompt must contain:
// the document ids the model is allowed to cite, and an explicit statement of
// the discard rule (stating the rule measurably improves compliance, and the
// server enforces it regardless).
func TestPromptCarriesWhitelistAndRule(t *testing.T) {
	c := &stubCompleter{enabled: true, reply: `{"sentences":[]}`}
	if _, err := Generate(context.Background(), c, testInput()); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if !strings.Contains(c.gotMessage, docA) || !strings.Contains(c.gotMessage, docB) {
		t.Fatal("prompt does not carry the action document ids the model must copy")
	}
	if !strings.Contains(c.gotSystem, "document_ids") {
		t.Fatal("system prompt does not state the required output schema")
	}
	if !strings.Contains(c.gotSystem, "폐기") {
		t.Fatal("system prompt does not state that ungrounded sentences are discarded")
	}
}

// TestPromptCapsActionCount pins that an unbounded open set cannot blow up the
// prompt (and its cost).
func TestPromptCapsActionCount(t *testing.T) {
	in := testInput()
	base := in.Actions[0]
	for i := 0; i < maxPromptActions+10; i++ {
		a := base
		a.IdentityKey = fmt.Sprintf("action:%016x", i)
		in.Actions = append(in.Actions, a)
	}

	c := &stubCompleter{enabled: true, reply: `{"sentences":[]}`}
	if _, err := Generate(context.Background(), c, in); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got := strings.Count(c.gotMessage, `"identity_key"`); got > maxPromptActions {
		t.Fatalf("prompt carried %d actions, want at most %d", got, maxPromptActions)
	}
}
