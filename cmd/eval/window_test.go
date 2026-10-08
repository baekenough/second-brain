package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/timeutil"
)

// --window=plan 은 골든 후보 화면이 쓰는 것과 같은 파서를 써야 한다. 여기서
// 두 경로가 갈라지면, 사람이 화면에서 1~2위로 본 문서가 평가에서는 묻히는
// 지금의 불일치가 이름만 바꾼 채 그대로 남는다.
func TestPlanWindowMatchesDeterministicParser(t *testing.T) {
	asOf := time.Date(2026, 9, 21, 9, 0, 0, 0, timeutil.KST())

	gotFrom, gotTo, ok := planWindowResolver("어제 뭐 했지", asOf)
	if !ok {
		t.Fatal("period phrase was not matched")
	}
	wantFrom, wantTo, _, wantOK := intent.DeterministicWindow("어제 뭐 했지", asOf.In(timeutil.KST()))
	if !wantOK || !gotFrom.Equal(wantFrom) || !gotTo.Equal(wantTo) {
		t.Fatalf("window = [%s, %s), want [%s, %s)", gotFrom, gotTo, wantFrom, wantTo)
	}
	// 어제(2026-09-20) 하루의 KST 반열린 구간이어야 한다.
	if gotFrom.In(timeutil.KST()).Format(time.RFC3339) != "2026-09-20T00:00:00+09:00" ||
		gotTo.In(timeutil.KST()).Format(time.RFC3339) != "2026-09-21T00:00:00+09:00" {
		t.Fatalf("unexpected KST day bounds: [%s, %s)", gotFrom, gotTo)
	}
}

// 기준 시각을 바꾸면 같은 문구가 다른 창으로 해석돼야 한다 — 기준 시각이
// 실제로 먹히는지의 증거다.
func TestPlanWindowAnchorsToAsOf(t *testing.T) {
	earlyFrom, _, okEarly := planWindowResolver("오늘 일정", time.Date(2026, 3, 5, 12, 0, 0, 0, timeutil.KST()))
	lateFrom, _, okLate := planWindowResolver("오늘 일정", time.Date(2026, 9, 21, 12, 0, 0, 0, timeutil.KST()))
	if !okEarly || !okLate {
		t.Fatal("period phrase was not matched")
	}
	if earlyFrom.Equal(lateFrom) {
		t.Fatalf("--as-of ignored: both resolved to %s", earlyFrom)
	}
}

// 기간 표현이 없는 질의는 시간창 없이 검색해야 한다. 여기서 임의의 창을
// 만들어 붙이면 평가가 운영보다 좁은 코퍼스를 재게 된다.
func TestPlanWindowLeavesUndatedQueriesUnconstrained(t *testing.T) {
	if _, _, ok := planWindowResolver("보험 약관 정리해줘", time.Date(2026, 9, 21, 12, 0, 0, 0, timeutil.KST())); ok {
		t.Fatal("a query with no period phrase received a window")
	}
}

// 해석된 창은 실제로 검색 질의의 occurred_at 범위까지 전달돼야 한다.
// 시간창을 "정렬 힌트" 로 흘려보내면 후보에 들어오지도 못한 문서를 정렬할 수
// 없으므로 아무것도 달라지지 않는다.
func TestWindowReachesTheSearchQuery(t *testing.T) {
	asOf := time.Date(2026, 9, 21, 9, 0, 0, 0, timeutil.KST())
	stub := newTracingStub(false)
	pairs := []store.EvalPair{
		{Query: "어제 통화", RelevantDocIDs: []string{labelDocID.String()}},
		{Query: "보험 약관", RelevantDocIDs: []string{labelDocID.String()}},
	}
	evaluatePairs(context.Background(), stub, pairs, evalRunOptions{
		window: planWindowResolver,
		anchor: windowAnchorFor(windowAnchorAsOf, asOf),
	})
	close(stub.lastQuery)

	windowed, unwindowed := 0, 0
	for q := range stub.lastQuery {
		switch {
		case q.OccurredFrom != nil && q.OccurredTo != nil:
			windowed++
			if !q.OccurredFrom.Before(*q.OccurredTo) {
				t.Fatalf("window is not a half-open range: [%s, %s)", q.OccurredFrom, q.OccurredTo)
			}
		case q.OccurredFrom == nil && q.OccurredTo == nil:
			unwindowed++
		default:
			t.Fatalf("half-set window: from=%v to=%v", q.OccurredFrom, q.OccurredTo)
		}
	}
	if windowed != 1 || unwindowed != 1 {
		t.Fatalf("windowed=%d unwindowed=%d, want 1 and 1", windowed, unwindowed)
	}
}

// 기본값(--window=none)은 예전 그대로 시간창 없이 검색한다.
func TestDefaultModeSendsNoWindow(t *testing.T) {
	stub := newTracingStub(false)
	evaluatePairs(context.Background(), stub,
		[]store.EvalPair{{Query: "어제 통화", RelevantDocIDs: []string{labelDocID.String()}}},
		evalRunOptions{})
	close(stub.lastQuery)
	for q := range stub.lastQuery {
		if q.OccurredFrom != nil || q.OccurredTo != nil {
			t.Fatalf("default mode applied a window: from=%v to=%v", q.OccurredFrom, q.OccurredTo)
		}
	}
}

// 시간창 모드는 설정 정체성의 일부다. plan 실행이 none 실행의 baseline 과
// 같은 해시로 묶이면, 시간창 덕에 오른 점수를 검색 품질 개선으로 읽게 된다.
func TestWindowModeFormsASeparateBaseline(t *testing.T) {
	asOf := time.Date(2026, 9, 21, 9, 0, 0, 0, timeutil.KST())
	base := map[string]any{"protocol": "x"}
	none := map[string]any{"protocol": "x"}

	applyWindowProfile(none, windowModeNone, "", asOf)
	if digest(none) != digest(base) {
		t.Fatal("default mode changed the config hash and invalidated existing baselines")
	}

	plan := map[string]any{"protocol": "x"}
	applyWindowProfile(plan, windowModePlan, windowAnchorAsOf, asOf)
	if digest(plan) == digest(base) {
		t.Fatal("plan mode reused the default baseline")
	}

	// 기준 날짜가 다르면 시간창도 다르므로 또 다른 baseline 이어야 한다.
	otherDay := map[string]any{"protocol": "x"}
	applyWindowProfile(otherDay, windowModePlan, windowAnchorAsOf, asOf.AddDate(0, 0, -1))
	if digest(otherDay) == digest(plan) {
		t.Fatal("a different as-of date reused the same baseline")
	}

	// 같은 날 안에서 시각만 다른 실행은 같은 시간창을 만든다 — 해시도 같아야
	// 한다. 초 단위를 넣으면 매 실행이 고유해져 baseline 이 영영 안 맞는다.
	sameDay := map[string]any{"protocol": "x"}
	applyWindowProfile(sameDay, windowModePlan, windowAnchorAsOf, asOf.Add(7*time.Hour))
	if digest(sameDay) != digest(plan) {
		t.Fatal("same KST day produced different baselines")
	}
}

// --as-of 방식(as_of)의 프로필은 이 변경 전과 같은 키 집합이어야 한다. 새 키를
// 넣으면 이미 쌓인 --as-of baseline 이 전부 해시 불일치가 된다.
func TestWindowProfile_AsOfKeysUnchanged(t *testing.T) {
	asOf := time.Date(2026, 9, 21, 9, 0, 0, 0, timeutil.KST())
	profile := map[string]any{}
	applyWindowProfile(profile, windowModePlan, windowAnchorAsOf, asOf)
	want := map[string]any{
		"window_mode":           "plan",
		"window_resolver":       "intent.DeterministicWindow",
		"window_as_of_kst_date": "2026-09-21",
	}
	if digest(profile) != digest(want) {
		t.Fatalf("as_of profile changed: %+v", profile)
	}
}

// asked_at/judged_at 는 각자 별도 baseline 계열이고, 실행 날짜에 흔들리지 않는다.
func TestWindowProfile_PerQueryAnchorsFormOwnSeries(t *testing.T) {
	day1 := time.Date(2026, 10, 8, 9, 0, 0, 0, timeutil.KST())
	day2 := day1.AddDate(0, 0, 3)
	profile := func(anchor string, asOf time.Time) map[string]any {
		p := map[string]any{"protocol": "x"}
		applyWindowProfile(p, windowModePlan, anchor, asOf)
		return p
	}

	asked, judged, asOfProfile := profile(windowAnchorAskedAt, day1), profile(windowAnchorJudgedAt, day1), profile(windowAnchorAsOf, day1)
	if asked["window_anchor"] != windowAnchorAskedAt || judged["window_anchor"] != windowAnchorJudgedAt {
		t.Fatalf("window_anchor 누락: %+v / %+v", asked, judged)
	}
	if _, ok := asked["window_as_of_kst_date"]; ok {
		t.Fatalf("asked_at 프로필에 실행 날짜가 들어갔다: %+v", asked)
	}
	if _, ok := asOfProfile["window_anchor"]; ok {
		t.Fatalf("as_of 프로필에 window_anchor 가 들어갔다: %+v", asOfProfile)
	}
	if digest(asked) == digest(judged) || digest(asked) == digest(asOfProfile) || digest(judged) == digest(asOfProfile) {
		t.Fatal("서로 다른 기준 출처가 같은 baseline 계열로 묶였다")
	}
	if digest(asked) != digest(profile(windowAnchorAskedAt, day2)) {
		t.Fatal("asked_at 기준인데 실행 날짜가 해시에 영향을 줬다")
	}
}

func TestResolveWindowAnchor(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		asOf     string
		anchor   string
		explicit bool
		want     string
		wantErr  bool
	}{
		{"none 기본", windowModeNone, "", windowAnchorAskedAt, false, "", false},
		{"none 인데 앵커 명시", windowModeNone, "", windowAnchorAskedAt, true, "", true},
		{"plan 플래그 기본값 전달", windowModePlan, "", windowAnchorJudgedAt, false, windowAnchorJudgedAt, false},
		{"plan judged_at", windowModePlan, "", windowAnchorJudgedAt, true, windowAnchorJudgedAt, false},
		{"--as-of 가 전역 덮어쓰기", windowModePlan, "2026-09-21T00:00:00+09:00", windowAnchorAskedAt, false, windowAnchorAsOf, false},
		{"--as-of 와 --window-anchor 동시 명시", windowModePlan, "2026-09-21T00:00:00+09:00", windowAnchorAskedAt, true, "", true},
		{"앵커 오타", windowModePlan, "", "asked", true, "", true},
	}
	for _, tc := range tests {
		got, err := resolveWindowAnchor(tc.mode, tc.asOf, tc.anchor, tc.explicit)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("%s: got (%q, %v), want (%q, err=%v)", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}

// 질의마다 자기 기준 시각으로 창이 풀리고, 다른 질의의 기준 시각이 섞이지 않는다.
// --as-of 로 고정하면 모든 질의가 그 시각 하나로 풀린다. 덤프에는 기준 날짜(KST)가
// 남는다.
func TestPerQueryAnchorResolution(t *testing.T) {
	kst := timeutil.KST()
	askedA := time.Date(2026, 9, 20, 23, 30, 0, 0, kst) // "내일" -> 9/21
	askedB := time.Date(2026, 8, 3, 10, 0, 0, 0, kst)   // "내일" -> 8/4
	judged := time.Date(2026, 9, 25, 10, 0, 0, 0, kst)
	asOf := time.Date(2026, 10, 8, 9, 0, 0, 0, kst)
	pairs := []store.EvalPair{
		{Query: "내일 일정", AskedAt: askedA, CreatedAt: judged, RelevantDocIDs: []string{labelDocID.String()}},
		{Query: "내일 회의", AskedAt: askedB, CreatedAt: judged, RelevantDocIDs: []string{labelDocID.String()}},
		{Query: "내일 점심", CreatedAt: judged, RelevantDocIDs: []string{labelDocID.String()}}, // asked_at 없음
	}

	run := func(kind string) map[string]time.Time {
		stub := newTracingStub(false)
		evaluatePairs(context.Background(), stub, pairs, evalRunOptions{
			window: planWindowResolver, anchor: windowAnchorFor(kind, asOf), diagnose: true,
		})
		close(stub.lastQuery)
		out := map[string]time.Time{}
		for q := range stub.lastQuery {
			if q.OccurredFrom == nil {
				t.Fatalf("%s: %q 에 창이 없다", kind, q.Query)
			}
			out[q.Query] = q.OccurredFrom.In(kst)
		}
		return out
	}
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, kst) }

	asked := run(windowAnchorAskedAt)
	if !asked["내일 일정"].Equal(day(2026, 9, 21)) || !asked["내일 회의"].Equal(day(2026, 8, 4)) {
		t.Errorf("asked_at 기준이 질의별로 적용되지 않았다: %v", asked)
	}
	// asked_at 이 없는 쌍(피드백 기반)은 CreatedAt 으로 내려간다.
	if !asked["내일 점심"].Equal(day(2026, 9, 26)) {
		t.Errorf("asked_at 없는 쌍의 폴백: %v", asked["내일 점심"])
	}
	for q, got := range run(windowAnchorJudgedAt) {
		if !got.Equal(day(2026, 9, 26)) {
			t.Errorf("judged_at 기준: %q -> %v", q, got)
		}
	}
	// --as-of 는 질의별 값을 모두 덮어쓴다.
	for q, got := range run(windowAnchorAsOf) {
		if !got.Equal(day(2026, 10, 9)) {
			t.Errorf("as_of 전역 기준: %q -> %v", q, got)
		}
	}
}

func TestDumpCarriesWindowAnchorDate(t *testing.T) {
	kst := timeutil.KST()
	asked := time.Date(2026, 9, 20, 23, 30, 0, 0, kst)
	stub := newTracingStub(false)
	got := evaluatePairs(context.Background(), stub, []store.EvalPair{
		{Query: "내일 일정", AskedAt: asked, RelevantDocIDs: []string{labelDocID.String()}},
		{Query: "보험 약관", AskedAt: asked, RelevantDocIDs: []string{labelDocID.String()}},
	}, evalRunOptions{
		window: planWindowResolver, anchor: windowAnchorFor(windowAnchorAskedAt, time.Now()), diagnose: true,
	})
	if len(got.Diagnostics) != 2 {
		t.Fatalf("diagnostics = %d", len(got.Diagnostics))
	}
	for i, d := range got.Diagnostics {
		if d.WindowAnchorDate != "2026-09-20" {
			t.Errorf("row %d: window_anchor_date = %q", i, d.WindowAnchorDate)
		}
	}
	if got.Diagnostics[0].WindowApplied == nil || got.Diagnostics[1].WindowApplied != nil {
		t.Errorf("window_applied: %+v / %+v", got.Diagnostics[0].WindowApplied, got.Diagnostics[1].WindowApplied)
	}

	// 시간창 모드가 none 이면 키 자체가 없다.
	none := evaluatePairs(context.Background(), newTracingStub(false),
		[]store.EvalPair{{Query: "내일 일정", RelevantDocIDs: []string{labelDocID.String()}}},
		evalRunOptions{diagnose: true})
	if none.Diagnostics[0].WindowAnchorDate != "" {
		t.Errorf("none 모드에 anchor date: %q", none.Diagnostics[0].WindowAnchorDate)
	}
}

// --plan-sources: 운영 /ask 의 결정론적 계획과 같은 소스 포함 집합을 질의마다 건다.
func TestPlanSources_AppliedPerQuery(t *testing.T) {
	kst := timeutil.KST()
	anchor := time.Date(2026, 9, 20, 10, 0, 0, 0, kst)
	pairs := []store.EvalPair{
		{Query: "내일 일정", CreatedAt: anchor, RelevantDocIDs: []string{labelDocID.String()}},        // 캘린더 키워드·미래 창
		{Query: "어제 통화 내역", CreatedAt: anchor, RelevantDocIDs: []string{labelDocID.String()}},     // 명시 기록 단어
		{Query: "지난주 회의록", CreatedAt: anchor, RelevantDocIDs: []string{labelDocID.String()}},      // 소스 단서 없음
		{Query: "내일 일정에 관한 메일", CreatedAt: anchor, RelevantDocIDs: []string{labelDocID.String()}}, // 계획이 거절 → 창만
		{Query: "보험 약관 정리", CreatedAt: anchor, RelevantDocIDs: []string{labelDocID.String()}},     // 기간 표현 없음
	}
	stub := newTracingStub(false)
	got := evaluatePairs(context.Background(), stub, pairs, evalRunOptions{
		window: planWindowResolver, anchor: windowAnchorFor(windowAnchorJudgedAt, time.Now()),
		planSrcs: true, diagnose: true,
	})
	close(stub.lastQuery)

	queries := map[string]model.SearchQuery{}
	for q := range stub.lastQuery {
		queries[q.Query] = q
	}
	srcNames := func(q model.SearchQuery) []string {
		var out []string
		for _, st := range q.SourceTypes {
			out = append(out, string(st))
		}
		return out
	}
	want := map[string][]string{
		"내일 일정":        {string(model.SourceCalendar)},
		"어제 통화 내역":     {string(model.SourceCall)},
		"지난주 회의록":      nil,
		"내일 일정에 관한 메일": nil,
		"보험 약관 정리":     nil,
	}
	for text, w := range want {
		if g := srcNames(queries[text]); !reflect.DeepEqual(g, w) {
			t.Errorf("%q: SourceTypes = %v, want %v", text, g, w)
		}
	}
	// 거절된 질의도 창은 DeterministicWindow 로 걸린다(현행 동작 유지).
	if q := queries["내일 일정에 관한 메일"]; q.OccurredFrom == nil || q.OccurredTo == nil {
		t.Error("계획이 거절한 질의에서 창이 사라졌다")
	}
	if q := queries["보험 약관 정리"]; q.OccurredFrom != nil || len(q.SourceTypes) != 0 {
		t.Errorf("기간 표현 없는 질의에 제약이 걸렸다: %+v", q)
	}

	// 덤프 행(입력 순서)에는 실제로 건 집합만 남는다.
	wantDump := [][]string{{"calendar"}, {"call"}, nil, nil, nil}
	for i, d := range got.Diagnostics {
		if !reflect.DeepEqual(d.PlanSources, wantDump[i]) {
			t.Errorf("row %d: plan_sources = %v, want %v", i, d.PlanSources, wantDump[i])
		}
	}
}

// 플래그를 끄면 소스 제약도 덤프 키도 없다(기존 --window=plan 동작 그대로).
func TestPlanSources_OffLeavesQueriesUnconstrained(t *testing.T) {
	stub := newTracingStub(false)
	got := evaluatePairs(context.Background(), stub,
		[]store.EvalPair{{Query: "내일 일정", CreatedAt: time.Date(2026, 9, 20, 10, 0, 0, 0, timeutil.KST()), RelevantDocIDs: []string{labelDocID.String()}}},
		evalRunOptions{window: planWindowResolver, anchor: windowAnchorFor(windowAnchorJudgedAt, time.Now()), diagnose: true})
	close(stub.lastQuery)
	for q := range stub.lastQuery {
		if len(q.SourceTypes) != 0 {
			t.Errorf("--plan-sources 없이 소스가 걸렸다: %v", q.SourceTypes)
		}
	}
	if got.Diagnostics[0].PlanSources != nil {
		t.Errorf("plan_sources = %v", got.Diagnostics[0].PlanSources)
	}
}

func TestPlanSources_FlagValidationAndProfile(t *testing.T) {
	if err := validatePlanSources(windowModeNone, true); err == nil {
		t.Error("--plan-sources 가 --window=none 에서 통과했다")
	}
	for _, c := range []struct {
		mode string
		on   bool
	}{{windowModePlan, true}, {windowModePlan, false}, {windowModeNone, false}} {
		if err := validatePlanSources(c.mode, c.on); err != nil {
			t.Errorf("(%s,%v): %v", c.mode, c.on, err)
		}
	}

	base := map[string]any{"protocol": "x"}
	applyWindowProfile(base, windowModePlan, windowAnchorJudgedAt, time.Now())
	off := map[string]any{"protocol": "x"}
	applyWindowProfile(off, windowModePlan, windowAnchorJudgedAt, time.Now())
	applyPlanSourcesProfile(off, false)
	if digest(off) != digest(base) {
		t.Error("--plan-sources 를 끈 실행이 config_hash 를 바꿨다")
	}
	on := map[string]any{"protocol": "x"}
	applyWindowProfile(on, windowModePlan, windowAnchorJudgedAt, time.Now())
	applyPlanSourcesProfile(on, true)
	if on["plan_sources"] != true || digest(on) == digest(base) {
		t.Errorf("plan_sources 가 별도 계열을 만들지 않았다: %+v", on)
	}
}

// --window-anchor 의 기본값은 judged_at 이다: 후보 화면이 리뷰 시각으로 창을
// 풀기 때문에 라벨이 붙은 순간의 창을 재현한다. asked_at 은 명시하면 고를 수 있다.
func TestWindowAnchorDefaultIsJudgedAt(t *testing.T) {
	got, err := resolveWindowAnchor(windowModePlan, "", windowAnchorJudgedAt, false)
	if err != nil || got != windowAnchorJudgedAt {
		t.Fatalf("got (%q, %v)", got, err)
	}
	if got, err := resolveWindowAnchor(windowModePlan, "", windowAnchorAskedAt, true); err != nil || got != windowAnchorAskedAt {
		t.Fatalf("asked_at 을 고를 수 없다: (%q, %v)", got, err)
	}
}
