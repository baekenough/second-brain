package intent_test

import (
	"context"
	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"strings"
	"testing"
)

type semanticCompleter struct {
	response string
	calls    int
	system   string
}

func (c *semanticCompleter) Enabled() bool { return true }
func (c *semanticCompleter) CompleteWithMessages(_ context.Context, system string, _ []llm.Message) (string, error) {
	c.calls++
	c.system = system
	return c.response, nil
}

func TestSemanticPlannerDoesNotDiscardSpecificity(t *testing.T) {
	cases := []struct {
		question, from, to string
		source             model.SourceType
	}{

		{"이번 주 금요일 일정", "2026-08-21", "2026-08-22", model.SourceCalendar},

		{"내일 일정에 대해 어제 받은 메일", "2026-08-18", "2026-08-19", model.SourceGmail},
		{"내일 일정에 관한 문자", "", "", model.SourceSMS},
		{"오늘 일정에 관한 메일", "", "", model.SourceGmail},
	}
	for _, tc := range cases {
		t.Run(tc.question, func(t *testing.T) {
			sources := "[]"
			if tc.source != "" {
				sources = `["` + string(tc.source) + `"]`
			}
			c := &semanticCompleter{response: `{"occurred_from":"` + tc.from + `","occurred_to":"` + tc.to + `","source_types":` + sources + `,"reason":"요청한 기록 조회"}`}
			got := newPlanner(t, c, planNowUTC).Plan(context.Background(), tc.question)
			if c.calls != 1 || got.Origin != intent.OriginLLM {
				t.Fatalf("must use semantic plan: calls=%d origin=%s", c.calls, got.Origin)
			}
			var wantSources []model.SourceType
			if tc.source != "" {
				wantSources = []model.SourceType{tc.source}
			}
			assertPlanMatches(t, planGoldenCase{question: tc.question, wantFrom: tc.from, wantTo: tc.to, wantSources: wantSources}, got)
			if !strings.Contains(c.system, "sent date is unknown") {
				t.Fatal("prompt must distinguish message time from discussed event time")
			}
		})
	}
}

func TestAmbiguousPlannerFailureDoesNotInventNarrowWindow(t *testing.T) {
	for _, q := range []string{"이번 주 금요일 일정", "지난달과 이번달 비교", "내일 일정에 관한 메일"} {
		got := newPlanner(t, nil, planNowUTC).Plan(context.Background(), q)
		if got.OccurredFrom != nil || got.OccurredTo != nil || len(got.SourceTypes) > 0 {
			t.Fatalf("unsafe narrowing for %q", q)
		}
	}
}

func TestExactDayAndSimpleSourceFastPath(t *testing.T) {
	cases := []planGoldenCase{
		{question: "2026년 8월 21일 일정", wantFrom: "2026-08-21", wantTo: "2026-08-22", wantSources: []model.SourceType{model.SourceCalendar}},
		{question: "2026-08-21 일정", wantFrom: "2026-08-21", wantTo: "2026-08-22", wantSources: []model.SourceType{model.SourceCalendar}},
		{question: "어제와 오늘 비교", wantFrom: "2026-08-18", wantTo: "2026-08-20"},
		{question: "오늘 통화 요약", wantFrom: "2026-08-19", wantTo: "2026-08-20", wantSources: []model.SourceType{model.SourceCall}},
		{question: "오늘 받은 약속 안내 문자", wantFrom: "2026-08-19", wantTo: "2026-08-20", wantSources: []model.SourceType{model.SourceSMS}},
	}
	for _, tc := range cases {
		t.Run(tc.question, func(t *testing.T) {
			c := &hostileCompleter{}
			got := newPlanner(t, c, planNowUTC).Plan(context.Background(), tc.question)
			assertPlanMatches(t, tc, got)
			if c.calls != 0 || got.Origin != intent.OriginDeterministic {
				t.Fatal("safe question left fast path")
			}
		})
	}
	for _, q := range []string{"2026년 2월 30일 일정", "2026-13-01 일정", "2026년 8월 21일부터 22일까지", "2026-08-21와 2026-08-23 비교"} {
		if _, _, _, ok := intent.DeterministicWindow(q, planNowUTC); ok {
			t.Fatalf("invalid/ambiguous date accepted: %s", q)
		}
	}
}
