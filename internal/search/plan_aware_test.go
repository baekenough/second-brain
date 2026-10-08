package search

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// --- 일정 의도 캘린더 가산 ---

func scheduleBoostFixture() []*model.SearchResult {
	return []*model.SearchResult{
		pfDoc(1, model.SourceCall, 0.040, pfAt(3, 9)),
		pfDoc(2, model.SourceGmail, 0.035, pfAt(3, 9)),
		pfDoc(3, model.SourceCalendar, 0.032, pfAt(4, 9)),
		pfDoc(4, model.SourceSMS, 0.030, pfAt(3, 9)),
		pfDoc(5, model.SourceCalendar, 0.010, pfAt(5, 9)),
		pfDoc(6, model.SourceCalendar, -0.5, pfAt(6, 9)), // 음수: 건드리지 않는다
		pfDoc(7, model.SourceCalendar, math.NaN(), nil),  // NaN: 건드리지 않는다
	}
}

func TestScheduleIntentBoost_OnlyCalendarMovesUp(t *testing.T) {
	t.Parallel()
	in := scheduleBoostFixture()
	q := model.SearchQuery{Query: "내일 뭐 해?"}
	on := model.SearchTuning{ScheduleIntentBoost: 0.5}.Normalized()
	got := applyScheduleIntentBoost(q, in, on)

	// 0.032*1.5=0.048 > 0.040 → 캘린더 3이 1위, 0.010*1.5=0.015 는 그대로 5위.
	assertSameIDs(t, pfIDs(got), []uuid.UUID{pfID(3), pfID(1), pfID(2), pfID(4), pfID(5), pfID(6), pfID(7)})
	assertSameIDs(t, pfSortedIDs(got), pfSortedIDs(in))
	pre := map[uuid.UUID]int{}
	for i, r := range in {
		pre[r.ID] = i
	}
	for i, r := range got {
		if r.SourceType != model.SourceCalendar && i < pre[r.ID] {
			t.Errorf("non-calendar doc %v moved up (%d → %d)", r.ID, pre[r.ID], i)
		}
	}
	// 비캘린더 문서끼리의 상대 순서는 그대로.
	var nonCal []uuid.UUID
	for _, r := range got {
		if r.SourceType != model.SourceCalendar {
			nonCal = append(nonCal, r.ID)
		}
	}
	assertSameIDs(t, nonCal, []uuid.UUID{pfID(1), pfID(2), pfID(4)})
	if got[5].Score != -0.5 || in[2].Score != 0.032 {
		t.Fatal("negative score boosted or input mutated")
	}
}

func TestScheduleIntentBoost_NoOps(t *testing.T) {
	t.Parallel()
	in := scheduleBoostFixture()
	cal := model.SourceCalendar
	on := model.SearchTuning{ScheduleIntentBoost: 1}.Normalized()
	for name, tc := range map[string]struct {
		q    model.SearchQuery
		tune model.SearchTuning
	}{
		"knob off":           {model.SearchQuery{Query: "내일 뭐 해?"}, model.SearchTuning{}},
		"no schedule intent": {model.SearchQuery{Query: "프로젝트 예산"}, on},
		"record question":    {model.SearchQuery{Query: "어제 회의 관련 메일"}, on},
		"include set":        {model.SearchQuery{Query: "내일 뭐 해?", SourceTypes: []model.SourceType{model.SourceCall}}, on},
		"singular include":   {model.SearchQuery{Query: "내일 뭐 해?", SourceType: &cal}, on},
		"recent sort":        {model.SearchQuery{Query: "내일 뭐 해?", Sort: model.SortRecent}, on},
	} {
		got := applyScheduleIntentBoost(tc.q, in, tc.tune)
		assertSameIDs(t, pfIDs(got), pfIDs(in))
		for i := range got {
			if got[i] != in[i] {
				t.Errorf("%s: result %d replaced", name, i)
			}
		}
	}
	if got := applyScheduleIntentBoost(model.SearchQuery{Query: "내일 뭐 해?"}, nil, on); len(got) != 0 {
		t.Error("empty input grew")
	}
}

func TestScheduleIntentBoost_PipelineCallSite(t *testing.T) {
	t.Parallel()
	run := func(tune model.SearchTuning) []uuid.UUID {
		st := &pfStore{results: scheduleBoostFixture()[:5]}
		got, err := NewService(st, disabledEmbedderForTrace{}).WithTuning(model.SearchTuning{}).
			Search(context.Background(), model.SearchQuery{Query: "다음 주 미팅 언제야", Limit: 5, Tuning: tune})
		if err != nil {
			t.Fatal(err)
		}
		return pfIDs(got)
	}
	assertSameIDs(t, run(model.SearchTuning{}), []uuid.UUID{pfID(1), pfID(2), pfID(3), pfID(4), pfID(5)})
	assertSameIDs(t, run(model.SearchTuning{ScheduleIntentBoost: 0.5}), []uuid.UUID{pfID(3), pfID(1), pfID(2), pfID(4), pfID(5)})
}

// --- 계획 소스 넘침 검색 ---

// spillStore 는 질의를 기록한다. 포함 집합이 있으면 그 소스 문서만, 없으면
// (넘침 검색) 제외 집합을 무시하고 전부 돌려준다 — 필터를 지키지 않는
// 저장소에서도 포함 소스가 섞이지 않는지 보기 위해서다.
type spillStore struct {
	mu      sync.Mutex
	calls   []model.SearchQuery
	docs    []*model.SearchResult
	failAll bool // 넘침 검색 실패
}

func (s *spillStore) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, q)
	s.mu.Unlock()
	inc := q.IncludeSourceTypes()
	if len(inc) == 0 && s.failAll {
		return nil, errors.New("synthetic spill failure")
	}
	var out []*model.SearchResult
	for _, r := range s.docs {
		if len(inc) > 0 && !slices.Contains(inc, r.SourceType) {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}

func (s *spillStore) snapshot() []model.SearchQuery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func spillDocs() []*model.SearchResult {
	return []*model.SearchResult{
		pfDoc(1, model.SourceSMS, 9, pfAt(3, 9)),
		pfDoc(2, model.SourceSMS, 8, pfAt(3, 10)),
		pfDoc(3, model.SourceGmail, 7, pfAt(3, 11)), // 계획이 놓친 정답
		pfDoc(4, model.SourceCall, 6, pfAt(3, 12)),
		pfDoc(5, model.SourceGmail, 5, pfAt(3, 13)),
	}
}

func spillQuery(fromPlan bool, k int) model.SearchQuery {
	return model.SearchQuery{
		Query:                 "어제 받은 거",
		Limit:                 5,
		SourceTypes:           []model.SourceType{model.SourceSMS},
		SourceIncludeFromPlan: fromPlan,
		OccurredFrom:          pfAt(3, 0),
		OccurredTo:            pfAt(4, 0),
		ExcludeSourceTypes:    []model.SourceType{model.SourceNote},
		ExcludeRetention:      []string{model.RetentionLow},
		Tuning:                model.SearchTuning{PlanSourceSpillK: k},
	}
}

func TestPlanSourceSpill_NeverWidensUserFilter(t *testing.T) {
	t.Parallel()
	st := &spillStore{docs: spillDocs()}
	got, err := NewService(st, disabledEmbedderForTrace{}).Search(context.Background(), spillQuery(false, 10))
	if err != nil {
		t.Fatal(err)
	}
	if calls := st.snapshot(); len(calls) != 1 {
		t.Fatalf("user include set triggered %d store calls", len(calls))
	}
	for _, r := range got {
		if r.SourceType != model.SourceSMS {
			t.Fatalf("user include set widened: got %s", r.SourceType)
		}
	}
	if spec, ok := planSpillArm(spillQuery(false, 10), 10); ok || spec.keep != nil {
		t.Fatal("planSpillArm accepted a user include set")
	}
}

func TestPlanSourceSpill_AddsOnlyNonIncludedSources(t *testing.T) {
	t.Parallel()
	st := &spillStore{docs: spillDocs()}
	q := spillQuery(true, 2)
	got, trace, err := NewService(st, disabledEmbedderForTrace{}).WithTuning(model.SearchTuning{}).
		SearchTraced(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	calls := st.snapshot()
	if len(calls) != 2 {
		t.Fatalf("store calls = %d, want 2", len(calls))
	}
	var spill model.SearchQuery
	for _, c := range calls {
		if len(c.IncludeSourceTypes()) == 0 {
			spill = c
		}
	}
	if spill.Limit != 2 || spill.Query != q.Query ||
		spill.OccurredFrom == nil || !spill.OccurredFrom.Equal(*q.OccurredFrom) ||
		spill.OccurredTo == nil || !spill.OccurredTo.Equal(*q.OccurredTo) ||
		!slices.Contains(spill.ExcludeRetention, model.RetentionLow) ||
		!slices.Contains(spill.ExcludeSourceTypes, model.SourceNote) ||
		!slices.Contains(spill.ExcludeSourceTypes, model.SourceSMS) {
		t.Fatalf("spill query lost a filter: limit=%d exclude=%v", spill.Limit, spill.ExcludeSourceTypes)
	}
	ids := pfIDs(got)
	if !slices.Contains(ids, pfID(3)) {
		t.Fatalf("mis-planned answer not reachable: %v", ids)
	}
	// 넘침 레인은 포함 소스(sms) 문서를 올리지 않고, K=2 를 넘지 않는다.
	var spilled []uuid.UUID
	for id, lanes := range trace.LaneHits {
		if slices.Contains(lanes, LanePlanSourceSpill) {
			spilled = append(spilled, id)
		}
	}
	assertSameIDs(t, pfSortIDs(spilled), pfSortIDs([]uuid.UUID{pfID(3), pfID(4)}))
	if len(trace.PoolIDs) != 4 { // sms 2 + 넘침 2
		t.Fatalf("pool = %d, want 4", len(trace.PoolIDs))
	}
}

func TestPlanSourceSpill_KnobOffIdentityAndFailureAbsorbed(t *testing.T) {
	t.Parallel()
	base := func(q model.SearchQuery, failAll bool) ([]uuid.UUID, int) {
		st := &spillStore{docs: spillDocs(), failAll: failAll}
		got, err := NewService(st, disabledEmbedderForTrace{}).WithTuning(model.SearchTuning{}).
			Search(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		return pfIDs(got), len(st.snapshot())
	}
	off, calls := base(spillQuery(true, 0), false)
	if calls != 1 {
		t.Fatalf("knob off made %d store calls", calls)
	}
	assertSameIDs(t, off, []uuid.UUID{pfID(1), pfID(2)})
	failed, calls := base(spillQuery(true, 3), true)
	if calls != 2 {
		t.Fatalf("spill not attempted: %d calls", calls)
	}
	assertSameIDs(t, failed, off)
}

// spillBlockingStore 는 넘침 검색을 ctx 가 끝날 때까지 붙잡고 본 검색을
// 실패시킨다.
type spillBlockingStore struct {
	started   chan struct{}
	cancelled chan struct{}
}

func (s *spillBlockingStore) Search(ctx context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	if len(q.IncludeSourceTypes()) > 0 {
		<-s.started
		return nil, errors.New("synthetic main failure")
	}
	s.started <- struct{}{}
	<-ctx.Done()
	s.cancelled <- struct{}{}
	return nil, ctx.Err()
}

func TestPlanSourceSpill_EarlyReturnCancels(t *testing.T) {
	t.Parallel()
	st := &spillBlockingStore{started: make(chan struct{}, 1), cancelled: make(chan struct{}, 1)}
	if _, err := NewService(st, disabledEmbedderForTrace{}).Search(context.Background(), spillQuery(true, 3)); err == nil {
		t.Fatal("main store failure swallowed")
	}
	select {
	case <-st.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("spill query outlived the request")
	}
}

func TestRerankInputCap_CountsSpill(t *testing.T) {
	t.Parallel()
	if got := rerankInputCap(20, model.SearchTuning{PlanSourceSpillK: 4}); got != 24 {
		t.Errorf("cap = %d, want 24", got)
	}
	if got := rerankInputCap(199, model.SearchTuning{PlanSourceSpillK: 10}); got != overfetchLimitCap {
		t.Errorf("cap = %d, want %d", got, overfetchLimitCap)
	}
}
