package calendarauto

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/jev"
	"github.com/baekenough/second-brain/internal/llm"
)

type fakeJev struct {
	state     string
	questions map[string]jev.Question
	answer    json.RawMessage
}

func (j *fakeJev) Classify(_ context.Context, state string, q map[string]jev.Question) (*jev.Response, error) {
	j.state = state
	j.questions = q
	return &jev.Response{Answers: map[string]json.RawMessage{"calendar_action": j.answer}}, nil
}

type fakeLLM struct {
	output   string
	system   string
	messages []llm.Message
}

func (*fakeLLM) Enabled() bool { return true }
func (l *fakeLLM) CompleteWithMessages(_ context.Context, system string, messages []llm.Message) (string, error) {
	l.system = system
	l.messages = messages
	return l.output, nil
}
func TestEvaluatorJevDecisionUsesSourceTimeAndProbabilities(t *testing.T) {
	w, s, _, _ := fixture()
	j := &fakeJev{answer: json.RawMessage(`{"choice":"create","probabilities":{"create":0.96}}`)}
	evaluator := NewEvaluator(j, nil)
	decision, err := evaluator.Decide(context.Background(), s.job.Document, w.cfg.Now())
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != "create" || decision.Confidence != .96 {
		t.Fatalf("%+v", decision)
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(j.state), &state); err != nil {
		t.Fatal(err)
	}
	if state["timezone"] != "Asia/Seoul" || state["source_occurred_at"] == nil || state["current_time"] != w.cfg.Now().Format(time.RFC3339) {
		t.Fatal(state)
	}
	if len(j.questions) != 1 || j.questions["calendar_action"].Type != jev.QuestionChoice {
		t.Fatal(j.questions)
	}
}
func TestEvaluatorStrictExtractionJSON(t *testing.T) {
	for _, output := range []string{"```json\n{}\n```", `{"summary":"A","attendees":["evil@example.com"]}`, `{} {}`, `{"start":42}`, "not json"} {
		t.Run(output, func(t *testing.T) {
			_, s, _, _ := fixture()
			e := NewEvaluator(nil, &fakeLLM{output: output})
			if _, err := e.Extract(context.Background(), s.job.Document, time.Now()); err == nil {
				t.Fatal("accepted invalid extraction")
			}
		})
	}
}
func TestEvaluatorNullAndPromptBoundaries(t *testing.T) {
	_, s, _, _ := fixture()
	l := &fakeLLM{output: "null"}
	e := NewEvaluator(nil, l)
	event, err := e.Extract(context.Background(), s.job.Document, time.Now())
	if err != nil || event != nil {
		t.Fatalf("%+v %v", event, err)
	}
	if !strings.Contains(l.system, "untrusted data") || !strings.Contains(l.system, "arrival") || !strings.Contains(l.system, "empty end string") {
		t.Fatal("missing extraction boundaries")
	}
	if len(l.messages) != 1 || l.messages[0].Role != "user" {
		t.Fatal("source not isolated")
	}
}

func TestEvaluatorUsesSelectedProbabilityNotConcentration(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		want         float64
	}{
		{"concentrated", `{"choice":"create","confidence":0.88,"probabilities":{"create":0.92,"skip":0.06,"uncertain":0.02}}`, .92},
		{"missing probabilities", `{"choice":"create","confidence":1}`, 0},
		{"missing selected probability", `{"choice":"create","confidence":1,"probabilities":{"skip":1}}`, 0},
		{"low probability", `{"choice":"create","confidence":0.99,"probabilities":{"create":0.5}}`, .5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s, _, _ := fixture()
			e := NewEvaluator(&fakeJev{answer: json.RawMessage(tc.answer)}, nil)
			d, err := e.Decide(context.Background(), s.job.Document, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if d.Confidence != tc.want {
				t.Fatalf("probability=%v want=%v", d.Confidence, tc.want)
			}
		})
	}
}
