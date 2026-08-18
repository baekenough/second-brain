package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/timeutil"
)

// stubPlanner returns a fixed plan and records the question it was asked to
// plan for, so tests can assert the planner saw the REWRITTEN (standalone)
// question rather than the user's raw follow-up wording.
type stubPlanner struct {
	plan          intent.QueryPlan
	gotQuestions  []string
	planCallCount int
}

func (s *stubPlanner) Plan(_ context.Context, question string) intent.QueryPlan {
	s.planCallCount++
	s.gotQuestions = append(s.gotQuestions, question)
	return s.plan
}

// TestAskHandler_SourcesEventCarriesPlanReasonAndOrigin: a 0-result answer with
// no stated rationale is indistinguishable from a bug (spec §3.1). Origin
// separates "the LLM path is down" from "the regex is wrong" from "the prompt
// is wrong"; Reason is the user-facing half of the same diagnosis.
func TestAskHandler_SourcesEventCarriesPlanReasonAndOrigin(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 8, 20, 0, 0, 0, 0, timeutil.KST())
	to := time.Date(2026, 8, 21, 0, 0, 0, 0, timeutil.KST())
	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceCalendar, "일정")}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"답변"}}
	srv := newAskTestServer(searcher, &fakeIntentClassifier{}, fakeLLM)
	planner := &stubPlanner{plan: intent.QueryPlan{
		OccurredFrom: &from,
		OccurredTo:   &to,
		SourceTypes:  []model.SourceType{model.SourceCalendar},
		Limit:        8,
		Reason:       "내일(8/20) 캘린더만 조회",
		Origin:       intent.OriginDeterministic,
	}}
	srv.queryPlanner = planner

	rr := doAskRequest(t, srv, nil, map[string]any{"question": "내일 일정 알려줘"}, "Bearer test-key")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	frames := parseSSEFrames(t, rr.Body.String())
	var sourcesFrame *sseFrame
	for i := range frames {
		if frames[i].event == "sources" {
			sourcesFrame = &frames[i]
			break
		}
	}
	if sourcesFrame == nil {
		t.Fatalf("no sources event; got %v", frameEvents(frames))
	}

	var payload askSourcesPayload
	if err := json.Unmarshal([]byte(sourcesFrame.data), &payload); err != nil {
		t.Fatalf("unmarshal sources payload: %v", err)
	}
	if payload.Plan == nil {
		t.Fatal("sources payload carries no plan")
	}
	if payload.Plan.Origin != intent.OriginDeterministic {
		t.Errorf("plan.origin = %q, want %q", payload.Plan.Origin, intent.OriginDeterministic)
	}
	if payload.Plan.Reason != "내일(8/20) 캘린더만 조회" {
		t.Errorf("plan.reason = %q", payload.Plan.Reason)
	}
	if payload.Plan.OccurredFrom != "2026-08-20" || payload.Plan.OccurredTo != "2026-08-21" {
		t.Errorf("plan window = [%q, %q), want [2026-08-20, 2026-08-21) in KST",
			payload.Plan.OccurredFrom, payload.Plan.OccurredTo)
	}
	if len(payload.Plan.SourceTypes) != 1 || payload.Plan.SourceTypes[0] != "calendar" {
		t.Errorf("plan.source_types = %v, want [calendar]", payload.Plan.SourceTypes)
	}

	// The wire field names are the contract; assert on the raw JSON too so a
	// rename cannot pass by satisfying the Go struct alone.
	var raw map[string]any
	if err := json.Unmarshal([]byte(sourcesFrame.data), &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	planRaw, ok := raw["plan"].(map[string]any)
	if !ok {
		t.Fatalf(`sources payload has no "plan" object: %v`, raw)
	}
	for _, key := range []string{"origin", "reason", "occurred_from", "occurred_to", "source_types"} {
		if _, ok := planRaw[key]; !ok {
			t.Errorf("plan object is missing %q: %v", key, planRaw)
		}
	}
}

// TestAskHandler_ZeroResultsStillExplainsThePlan: the no_evidence path is
// exactly where the explanation matters most — "내일 일정 없습니다" must be
// distinguishable from a broken retriever.
func TestAskHandler_ZeroResultsStillExplainsThePlan(t *testing.T) {
	t.Parallel()

	searcher := &fixedDocSearcher{observed: nil}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"안 씀"}}
	srv := newAskTestServer(searcher, &fakeIntentClassifier{}, fakeLLM)
	srv.queryPlanner = &stubPlanner{plan: intent.QueryPlan{
		Limit:  8,
		Reason: "시간·소스 제약 없이 전체 검색",
		Origin: intent.OriginFallback,
	}}

	rr := doAskRequest(t, srv, nil, map[string]any{"question": "아무 문서도 없는 질문"}, "Bearer test-key")

	frames := parseSSEFrames(t, rr.Body.String())
	if got, want := frameEvents(frames), []string{"conversation", "sources", "done"}; len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	var payload askSourcesPayload
	if err := json.Unmarshal([]byte(frames[1].data), &payload); err != nil {
		t.Fatalf("unmarshal sources payload: %v", err)
	}
	if payload.Plan == nil || payload.Plan.Origin != intent.OriginFallback {
		t.Fatalf("0-result answer carries no fallback plan: %+v", payload.Plan)
	}
	if payload.Plan.OccurredFrom != "" || payload.Plan.OccurredTo != "" {
		t.Errorf("fallback plan reported a window [%q, %q)", payload.Plan.OccurredFrom, payload.Plan.OccurredTo)
	}
	if fakeLLM.streamCalls != 0 {
		t.Errorf("synthesis ran on 0 observed results (streamCalls=%d)", fakeLLM.streamCalls)
	}
}

// TestAskHandler_PlannerSeesRewrittenQuestion: Stage 1 runs on the standalone
// rewrite, not the raw follow-up. A planner fed "그거 더 자세히" can only produce
// a fallback plan, which would silently disable the feature for every
// multi-turn conversation.
func TestAskHandler_PlannerSeesRewrittenQuestion(t *testing.T) {
	t.Parallel()

	searcher := &fixedDocSearcher{observed: []*model.SearchResult{docResult(model.SourceSMS, "문자")}}
	fakeLLM := &fakeAskLLM{enabled: true, chunks: []string{"답변"}}
	srv := newAskTestServer(searcher, &fakeIntentClassifier{}, fakeLLM)
	planner := &stubPlanner{plan: intent.QueryPlan{Limit: 8, Reason: "제약 없음", Origin: intent.OriginFallback}}
	srv.queryPlanner = planner

	doAskRequest(t, srv, nil, map[string]any{"question": "내일 일정 알려줘"}, "Bearer test-key")

	if planner.planCallCount != 1 {
		t.Fatalf("planner called %d times, want exactly 1 per request", planner.planCallCount)
	}
	if planner.gotQuestions[0] != "내일 일정 알려줘" {
		t.Errorf("planner saw %q, want the search question", planner.gotQuestions[0])
	}
}
