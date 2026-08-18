package intent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/timeutil"
)

// ---------------------------------------------------------------------------
// Golden set — design spec §8.
//
// Reference instant: 2026-08-19(Wed) 10:00 KST, expressed here as 01:00 UTC.
// The UTC spelling is deliberate and load-bearing: the server/collector
// containers run as Etc/UTC, so a planner that resolved calendar boundaries in
// the clock's own location would produce windows nine hours off. Every expected
// bound below is a KST midnight (timeutil.KST()), never a local-machine one.
// A KST-only test asserts a proposition production never evaluates.
// ---------------------------------------------------------------------------

// planNowUTC is 2026-08-19 10:00 KST written in UTC.
var planNowUTC = time.Date(2026, 8, 19, 1, 0, 0, 0, time.UTC)

// planLimit is the planner's configured default Limit; every plan (including
// the fallback) must carry it, since a plan with Limit 0 would silently ask
// the store for nothing.
const planLimit = 8

// kstMidnight parses "YYYY-MM-DD" as 00:00:00 in timeutil.KST().
func kstMidnight(t *testing.T, date string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02", date, timeutil.KST())
	if err != nil {
		t.Fatalf("kstMidnight(%q): %v", date, err)
	}
	return parsed
}

// planGoldenCase is one row of spec §8's table. Empty wantFrom/wantTo mean
// "no window" (nil bounds); a nil wantSources means "no source filter".
type planGoldenCase struct {
	num           int
	question      string
	wantFrom      string
	wantTo        string
	wantSources   []model.SourceType
	deterministic bool // spec §8.1: rows 1-9 must not reach the LLM
}

// planGolden is spec §8's 14-row table, verbatim. The questions use only the
// synthetic identifiers the spec itself fixes (홍길동, 010-0000-0000).
var planGolden = []planGoldenCase{
	{1, "오늘 일정 알려줘", "2026-08-19", "2026-08-20", []model.SourceType{model.SourceCalendar}, true},
	{2, "어제 뭐 했지", "2026-08-18", "2026-08-19", nil, true},
	{3, "내일 일정 알려줘", "2026-08-20", "2026-08-21", []model.SourceType{model.SourceCalendar}, true},
	{4, "모레 회의 있나", "2026-08-21", "2026-08-22", []model.SourceType{model.SourceCalendar}, true},
	{5, "이번 주말에 뭐 하지", "2026-08-22", "2026-08-24", []model.SourceType{model.SourceCalendar}, true},
	{6, "다음 주 일정 정리해줘", "2026-08-24", "2026-08-31", []model.SourceType{model.SourceCalendar}, true},
	{7, "이번 주 요약해줘", "2026-08-17", "2026-08-24", nil, true},
	{8, "지난달에 뭐 있었어", "2026-07-01", "2026-08-01", nil, true},
	{9, "2026년 6월 요약", "2026-06-01", "2026-07-01", nil, true},
	{10, "홍길동한테 뭐라고 했더라", "", "", nil, false},
	{11, "홍길동이랑 통화한 내용", "", "", []model.SourceType{model.SourceCallTranscript, model.SourceCallLog}, false},
	{12, "프로젝트 진행 상황 어때", "", "", nil, false},
	{13, "최근에 받은 메일 정리", "", "", []model.SourceType{model.SourceGmail}, false},
	{14, "010-0000-0000 누구야", "", "", nil, false},
}

// assertPlanMatches applies spec §8.1's judging rules: instant-exact bounds,
// order-insensitive set equality on sources, non-empty Reason.
func assertPlanMatches(t *testing.T, c planGoldenCase, got intent.QueryPlan) {
	t.Helper()

	if c.wantFrom == "" {
		if got.OccurredFrom != nil {
			t.Errorf("#%d %q: OccurredFrom = %v, want nil", c.num, c.question, got.OccurredFrom)
		}
	} else {
		want := kstMidnight(t, c.wantFrom)
		if got.OccurredFrom == nil || !got.OccurredFrom.Equal(want) {
			t.Errorf("#%d %q: OccurredFrom = %v, want %v", c.num, c.question, got.OccurredFrom, want)
		}
	}
	if c.wantTo == "" {
		if got.OccurredTo != nil {
			t.Errorf("#%d %q: OccurredTo = %v, want nil", c.num, c.question, got.OccurredTo)
		}
	} else {
		want := kstMidnight(t, c.wantTo)
		if got.OccurredTo == nil || !got.OccurredTo.Equal(want) {
			t.Errorf("#%d %q: OccurredTo = %v, want %v", c.num, c.question, got.OccurredTo, want)
		}
	}

	if !sameSourceSet(got.SourceTypes, c.wantSources) {
		t.Errorf("#%d %q: SourceTypes = %v, want %v (set equality)", c.num, c.question, got.SourceTypes, c.wantSources)
	}
	if strings.TrimSpace(got.Reason) == "" {
		t.Errorf("#%d %q: Reason is empty; every plan must carry a one-line rationale", c.num, c.question)
	}
	if got.Limit != planLimit {
		t.Errorf("#%d %q: Limit = %d, want %d", c.num, c.question, got.Limit, planLimit)
	}
}

func sameSourceSet(got, want []model.SourceType) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[model.SourceType]int, len(got))
	for _, st := range got {
		seen[st]++
	}
	for _, st := range want {
		seen[st]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// hostileCompleter fails on every call. Deterministic-path tests inject it so
// that any LLM round-trip shows up as a test failure rather than a silent
// fallback — "the regex matched" and "the LLM guessed the same thing" must not
// be indistinguishable.
type hostileCompleter struct{ calls int }

func (h *hostileCompleter) Enabled() bool { return true }
func (h *hostileCompleter) CompleteWithMessages(context.Context, string, []llm.Message) (string, error) {
	h.calls++
	return "", errors.New("LLM must not be called on the deterministic path")
}

func newPlanner(t *testing.T, c llm.Completer, now time.Time) *intent.LLMPlanner {
	t.Helper()
	p := intent.NewLLMPlanner(c, planLimit)
	intent.SetPlannerNow(p, func() time.Time { return now })
	return p
}

// --- Deterministic path -----------------------------------------------------

func TestPlanner_GoldenSet_DeterministicPath(t *testing.T) {
	t.Parallel()

	for _, c := range planGolden {
		if !c.deterministic {
			continue
		}
		t.Run(fmt.Sprintf("%02d_%s", c.num, c.question), func(t *testing.T) {
			t.Parallel()
			hostile := &hostileCompleter{}
			got := newPlanner(t, hostile, planNowUTC).Plan(context.Background(), c.question)

			if hostile.calls != 0 {
				t.Errorf("LLM called %d times for a deterministic phrase", hostile.calls)
			}
			if got.Origin != intent.OriginDeterministic {
				t.Errorf("Origin = %q, want %q", got.Origin, intent.OriginDeterministic)
			}
			assertPlanMatches(t, c, got)
		})
	}
}

// --- LLM path ---------------------------------------------------------------

// planOracle is an httptest-backed stand-in for the remote model: it answers
// each golden question with that row's expected plan, encoded in the wire
// schema. It proves the parse/validate/normalise half of the LLM path — the
// half this repository owns — without spending money on a real call. It does
// NOT prove the model would answer this way; that is what a live golden run
// would measure, and it is deliberately out of scope here.
type planOracle struct {
	srv         *httptest.Server
	calls       int
	lastSystem  string
	lastMessage string
}

func newPlanOracle(t *testing.T, answer func(question string) string) *planOracle {
	t.Helper()
	o := &planOracle{}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("planOracle: decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		o.calls++
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				o.lastSystem = m.Content
			case "user":
				o.lastMessage = m.Content
			}
		}
		body, err := json.Marshal(map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": answer(o.lastMessage)},
				"finish_reason": "stop",
			}},
		})
		if err != nil {
			t.Errorf("planOracle: marshal response: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *planOracle) client() *llm.Client {
	return llm.New(llm.Config{
		BaseURL:   o.srv.URL,
		Model:     "planner-test",
		APIKey:    "test-key",
		MaxTokens: 750,
	}, nil)
}

// goldenAnswer encodes a golden row as the JSON the planner must accept.
func goldenAnswer(c planGoldenCase) string {
	sources := make([]string, 0, len(c.wantSources))
	for _, st := range c.wantSources {
		sources = append(sources, string(st))
	}
	payload, err := json.Marshal(map[string]any{
		"occurred_from": c.wantFrom,
		"occurred_to":   c.wantTo,
		"source_types":  sources,
		"reason":        fmt.Sprintf("%d번 계획", c.num),
	})
	if err != nil {
		panic(err)
	}
	return string(payload)
}

// TestPlanner_GoldenSet_LLMPath runs the SAME table through the LLM path, with
// the deterministic pre-pass disabled (spec §4.3: the regex layer is a cache,
// not a second semantics; both paths must satisfy one table).
func TestPlanner_GoldenSet_LLMPath(t *testing.T) {
	t.Parallel()

	byQuestion := make(map[string]planGoldenCase, len(planGolden))
	for _, c := range planGolden {
		byQuestion[c.question] = c
	}

	for _, c := range planGolden {
		t.Run(fmt.Sprintf("%02d_%s", c.num, c.question), func(t *testing.T) {
			t.Parallel()
			oracle := newPlanOracle(t, func(question string) string {
				row, ok := byQuestion[question]
				if !ok {
					t.Errorf("oracle got an unknown question %q", question)
					return "{}"
				}
				return goldenAnswer(row)
			})
			p := newPlanner(t, oracle.client(), planNowUTC)
			intent.DisableDeterministicPath(p)

			got := p.Plan(context.Background(), c.question)

			if oracle.calls != 1 {
				t.Errorf("LLM called %d times, want exactly 1 (spec §2.3 invariant)", oracle.calls)
			}
			if got.Origin != intent.OriginLLM {
				t.Errorf("Origin = %q, want %q", got.Origin, intent.OriginLLM)
			}
			assertPlanMatches(t, c, got)
		})
	}
}

// TestPlanner_LLMPrompt_CarriesKSTNow pins spec §4.2's hard requirement: the
// model cannot resolve "내일" without being told what today is, and it must be
// told in KST even though the process clock is UTC.
func TestPlanner_LLMPrompt_CarriesKSTNow(t *testing.T) {
	t.Parallel()

	// 2026-08-19 15:30 UTC is 2026-08-20 00:30 KST — the UTC and KST calendar
	// dates differ, so a prompt built from the raw clock says the wrong day.
	now := time.Date(2026, 8, 19, 15, 30, 0, 0, time.UTC)
	oracle := newPlanOracle(t, func(string) string { return `{"reason":"무제약"}` })
	p := newPlanner(t, oracle.client(), now)

	_ = p.Plan(context.Background(), "프로젝트 진행 상황 어때")

	if !strings.Contains(oracle.lastSystem, "2026-08-20") {
		t.Errorf("system prompt does not state the KST date 2026-08-20; got:\n%s", oracle.lastSystem)
	}
	if strings.Contains(oracle.lastSystem, "2026-08-19") {
		t.Errorf("system prompt states the UTC date 2026-08-19, which is not today in KST; got:\n%s", oracle.lastSystem)
	}
	if !strings.Contains(oracle.lastSystem, "calendar") || !strings.Contains(oracle.lastSystem, "call-transcript") {
		t.Errorf("system prompt does not enumerate the allowed source types; got:\n%s", oracle.lastSystem)
	}
}

// --- Failure handling (spec §6) --------------------------------------------

func assertFallback(t *testing.T, got intent.QueryPlan) {
	t.Helper()
	if got.Origin != intent.OriginFallback {
		t.Errorf("Origin = %q, want %q", got.Origin, intent.OriginFallback)
	}
	if got.OccurredFrom != nil || got.OccurredTo != nil {
		t.Errorf("fallback plan carries a window (%v, %v); spec §6.1 requires none", got.OccurredFrom, got.OccurredTo)
	}
	if len(got.SourceTypes) != 0 {
		t.Errorf("fallback plan carries source filters %v; spec §6.1 requires none", got.SourceTypes)
	}
	if strings.TrimSpace(got.Reason) == "" {
		t.Error("fallback plan has an empty Reason; the user must be told the search was unconstrained")
	}
	if got.Limit != planLimit {
		t.Errorf("fallback Limit = %d, want %d", got.Limit, planLimit)
	}
}

func TestPlanner_LLMFailureModes_FallBack(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		answer string
		err    error
	}{
		{name: "transport error", err: errors.New("dial tcp: connection refused")},
		{name: "context deadline", err: context.DeadlineExceeded},
		{name: "truncated", err: &llm.TruncatedError{FinishReason: "length", CompletionTokens: 750, ContentLength: 0}},
		{name: "empty body", answer: ""},
		{name: "not json", answer: "죄송합니다, 계획을 세울 수 없습니다."},
		{name: "unknown source type", answer: `{"source_types":["carrier-pigeon"],"reason":"x"}`},
		{name: "inverted window", answer: `{"occurred_from":"2026-08-20","occurred_to":"2026-08-19","reason":"x"}`},
		{name: "empty window", answer: `{"occurred_from":"2026-08-20","occurred_to":"2026-08-20","reason":"x"}`},
		{name: "unparsable date", answer: `{"occurred_from":"내일","occurred_to":"","reason":"x"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeCompleter{response: tc.answer, err: tc.err}
			got := newPlanner(t, fake, planNowUTC).Plan(context.Background(), "시간 표현 없는 질문")
			if fake.calls != 1 {
				t.Errorf("LLM calls = %d, want 1 (no retry: spec §4.2)", fake.calls)
			}
			assertFallback(t, got)
		})
	}
}

func TestPlanner_NoCompleter_FallsBackWithoutPanicking(t *testing.T) {
	t.Parallel()

	got := newPlanner(t, nil, planNowUTC).Plan(context.Background(), "시간 표현 없는 질문")
	assertFallback(t, got)
}

func TestPlanner_DisabledCompleter_DoesNotCall(t *testing.T) {
	t.Parallel()

	fake := &disabledCompleter{}
	got := newPlanner(t, fake, planNowUTC).Plan(context.Background(), "시간 표현 없는 질문")
	if fake.calls != 0 {
		t.Errorf("LLM calls = %d on a disabled client, want 0", fake.calls)
	}
	assertFallback(t, got)
}

type disabledCompleter struct{ calls int }

func (d *disabledCompleter) Enabled() bool { return false }
func (d *disabledCompleter) CompleteWithMessages(context.Context, string, []llm.Message) (string, error) {
	d.calls++
	return "", nil
}

// TestPlanner_NeverPlansInsightOnly guards the interaction with askHandler's
// no_evidence rule: an insight-only plan empties the observed lane, and an
// empty observed lane is reported as "no evidence" no matter how much insight
// material was retrieved. The planner therefore never emits insight at all.
func TestPlanner_NeverPlansInsightOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		answer      string
		wantSources []model.SourceType
	}{
		{
			name:        "insight only collapses to no filter",
			answer:      `{"source_types":["insight"],"reason":"추론만"}`,
			wantSources: nil,
		},
		{
			name:        "insight is stripped from a mixed set",
			answer:      `{"source_types":["insight","gmail"],"reason":"추론+메일"}`,
			wantSources: []model.SourceType{model.SourceGmail},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeCompleter{response: tc.answer}
			got := newPlanner(t, fake, planNowUTC).Plan(context.Background(), "시간 표현 없는 질문")
			if !sameSourceSet(got.SourceTypes, tc.wantSources) {
				t.Errorf("SourceTypes = %v, want %v", got.SourceTypes, tc.wantSources)
			}
			for _, st := range got.SourceTypes {
				if st == model.SourceInsight {
					t.Fatalf("plan names insight: %v", got.SourceTypes)
				}
			}
			if strings.TrimSpace(got.Reason) == "" {
				t.Error("Reason is empty")
			}
		})
	}
}

// TestPlanner_InsightOnlyWideningIsDisclosed: dropping the only requested
// source widens the search to the whole corpus. Widening is permitted only
// when it is stated (spec §6.2), so the Reason must change too.
func TestPlanner_InsightOnlyWideningIsDisclosed(t *testing.T) {
	t.Parallel()

	fake := &fakeCompleter{response: `{"source_types":["insight"],"reason":"추론만 조회"}`}
	got := newPlanner(t, fake, planNowUTC).Plan(context.Background(), "시간 표현 없는 질문")
	if got.Reason == "추론만 조회" {
		t.Error("Reason still claims an insight-only search after the filter was dropped")
	}
	if !strings.Contains(got.Reason, "전체") {
		t.Errorf("Reason does not disclose the widening to all sources: %q", got.Reason)
	}
}

// --- Timezone invariance (production runs UTC) ------------------------------

// TestPlanner_WindowsAreKSTUnderAnyClockZone kills the whole class rather than
// one instance: the same instant, expressed in three zones, must yield one
// window. The 2026-08-19 15:30 UTC instant is already 2026-08-20 in KST, so a
// zone-naive implementation answers a different DAY, not merely a different
// offset.
func TestPlanner_WindowsAreKSTUnderAnyClockZone(t *testing.T) {
	t.Parallel()

	instant := time.Date(2026, 8, 19, 15, 30, 0, 0, time.UTC)
	zones := []*time.Location{time.UTC, timeutil.KST(), time.FixedZone("UTC-8", -8*60*60)}

	var first intent.QueryPlan
	for i, loc := range zones {
		got := newPlanner(t, &hostileCompleter{}, instant.In(loc)).Plan(context.Background(), "오늘 일정 알려줘")
		if i == 0 {
			first = got
			wantFrom := kstMidnight(t, "2026-08-20")
			if got.OccurredFrom == nil || !got.OccurredFrom.Equal(wantFrom) {
				t.Fatalf("OccurredFrom = %v, want %v (KST day of 2026-08-19T15:30Z)", got.OccurredFrom, wantFrom)
			}
			continue
		}
		if !got.OccurredFrom.Equal(*first.OccurredFrom) || !got.OccurredTo.Equal(*first.OccurredTo) {
			t.Errorf("zone %s: window [%v, %v) differs from UTC's [%v, %v)",
				loc, got.OccurredFrom, got.OccurredTo, first.OccurredFrom, first.OccurredTo)
		}
	}
}

// TestPlanner_MonthEdgeUnderUTCClock is the harsher variant: at 2026-08-31
// 20:00 UTC it is already 2026-09-01 in KST, so "지난달" means August, not July.
func TestPlanner_MonthEdgeUnderUTCClock(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 31, 20, 0, 0, 0, time.UTC)
	got := newPlanner(t, &hostileCompleter{}, now).Plan(context.Background(), "지난달에 뭐 있었어")

	wantFrom, wantTo := kstMidnight(t, "2026-08-01"), kstMidnight(t, "2026-09-01")
	if got.OccurredFrom == nil || !got.OccurredFrom.Equal(wantFrom) {
		t.Errorf("OccurredFrom = %v, want %v", got.OccurredFrom, wantFrom)
	}
	if got.OccurredTo == nil || !got.OccurredTo.Equal(wantTo) {
		t.Errorf("OccurredTo = %v, want %v", got.OccurredTo, wantTo)
	}
}

// TestPlanner_ConsecutiveWindowsTile: today's upper bound is tomorrow's lower
// bound. A gap would put an event in neither day's window, which is how the
// inclusive-23:59:59 bug behaved once the bound became load-bearing.
func TestPlanner_ConsecutiveWindowsTile(t *testing.T) {
	t.Parallel()

	p := newPlanner(t, &hostileCompleter{}, planNowUTC)
	today := p.Plan(context.Background(), "오늘 일정 알려줘")
	tomorrow := p.Plan(context.Background(), "내일 일정 알려줘")
	if today.OccurredTo == nil || tomorrow.OccurredFrom == nil {
		t.Fatal("missing bounds")
	}
	if !today.OccurredTo.Equal(*tomorrow.OccurredFrom) {
		t.Errorf("오늘.To = %v but 내일.From = %v; consecutive windows must tile exactly",
			today.OccurredTo, tomorrow.OccurredFrom)
	}
}

// TestPlanner_FutureWindowsAreNotClamped: the planner is the first component in
// this system that emits a window whose bounds are entirely in the future.
func TestPlanner_FutureWindowsAreNotClamped(t *testing.T) {
	t.Parallel()

	now := planNowUTC
	for _, q := range []string{"내일 일정 알려줘", "모레 회의 있나", "이번 주말에 뭐 하지", "다음 주 일정 정리해줘"} {
		got := newPlanner(t, &hostileCompleter{}, now).Plan(context.Background(), q)
		if got.OccurredFrom == nil || got.OccurredTo == nil {
			t.Fatalf("%q: expected a window, got (%v, %v)", q, got.OccurredFrom, got.OccurredTo)
		}
		if !got.OccurredFrom.After(now) {
			t.Errorf("%q: OccurredFrom = %v is not in the future relative to %v", q, got.OccurredFrom, now)
		}
	}
}

// TestPlanner_NoTimePhrase_NoSilentWindow is spec §12's R5 in test form: the
// planner must not invent a window where the question states none. Inventing
// one is the mirror image of the bug this feature exists to fix.
func TestPlanner_NoTimePhrase_NoSilentWindow(t *testing.T) {
	t.Parallel()

	fake := &fakeCompleter{response: `{"occurred_from":"","occurred_to":"","source_types":["gmail"],"reason":"메일 전체"}`}
	got := newPlanner(t, fake, planNowUTC).Plan(context.Background(), "최근에 받은 메일 정리")
	if got.OccurredFrom != nil || got.OccurredTo != nil {
		t.Errorf("window = [%v, %v), want none", got.OccurredFrom, got.OccurredTo)
	}
}
