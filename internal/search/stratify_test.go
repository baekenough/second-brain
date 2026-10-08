package search

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// armStore 는 동시 호출을 기록하는 저장소 더블이다. 소스 포함 집합이 하나인
// 호출(보조 검색)은 그 소스 문서를, 아니면 본 검색 결과를 돌려준다. failFor 에
// 든 소스의 보조 검색은 실패한다.
type armStore struct {
	mu      sync.Mutex
	calls   []model.SearchQuery
	global  []*model.SearchResult
	bySrc   map[model.SourceType][]*model.SearchResult
	failFor map[model.SourceType]bool
}

func (s *armStore) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, q)
	s.mu.Unlock()
	inc := q.IncludeSourceTypes()
	if len(inc) == 0 {
		return cloneResults(s.global), nil
	}
	if s.failFor[inc[0]] {
		return nil, errors.New("synthetic arm failure")
	}
	res := cloneResults(s.bySrc[inc[0]])
	if len(res) > q.Limit {
		res = res[:q.Limit]
	}
	return res, nil
}

func cloneResults(in []*model.SearchResult) []*model.SearchResult {
	out := make([]*model.SearchResult, len(in))
	for i, r := range in {
		cp := *r
		out[i] = &cp
	}
	return out
}

func (s *armStore) armCalls() []model.SearchQuery {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.SearchQuery
	for _, c := range s.calls {
		if len(c.IncludeSourceTypes()) > 0 {
			out = append(out, c)
		}
	}
	return out
}

func armFixture() *armStore {
	global := []*model.SearchResult{
		pfDoc(1, model.SourceGmail, 3, pfAt(2, 9)),
		pfDoc(2, model.SourceGmail, 2, pfAt(3, 9)),
		pfDoc(3, model.SourceSMS, 1, pfAt(4, 9)),
	}
	return &armStore{
		global: global,
		bySrc: map[model.SourceType][]*model.SearchResult{
			model.SourceCalendar: {
				pfDoc(10, model.SourceCalendar, 5, pfAt(5, 9)),
				pfDoc(11, model.SourceCalendar, 4, pfAt(6, 9)),
				pfDoc(12, model.SourceCalendar, 3, pfAt(7, 9)),
			},
			model.SourceSMS: {
				pfDoc(3, model.SourceSMS, 9, pfAt(4, 9)), // 전역에도 있는 문서
				pfDoc(13, model.SourceSMS, 8, pfAt(4, 10)),
			},
			// 필터를 무시하는 저장소: 다른 소스 문서가 섞여 온다.
			model.SourceNote: {
				pfDoc(14, model.SourceGmail, 7, nil),
				pfDoc(15, model.SourceNote, 6, nil),
			},
		},
		failFor: map[model.SourceType]bool{model.SourceGmail: true},
	}
}

func TestSourceStratify_ArmQueriesInheritFilters(t *testing.T) {
	t.Parallel()
	st := armFixture()
	svc := NewService(st, disabledEmbedderForTrace{}).WithTuning(model.SearchTuning{})
	q := model.SearchQuery{
		Query:              "q",
		Limit:              5,
		OccurredFrom:       pfAt(1, 0),
		OccurredTo:         pfAt(30, 0),
		ExcludeSourceTypes: []model.SourceType{model.SourceCall},
		ExcludeRetention:   []string{model.RetentionLow},
		Tuning:             model.SearchTuning{SourceStratifyK: 2},
	}
	got, trace, err := svc.SearchTraced(context.Background(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	arms := st.armCalls()
	var sources []model.SourceType
	for _, c := range arms {
		inc := c.IncludeSourceTypes()
		if len(inc) != 1 {
			t.Fatalf("arm include set = %v, want exactly one source", inc)
		}
		sources = append(sources, inc[0])
		if c.Limit != 2 {
			t.Errorf("arm %s limit = %d, want 2", inc[0], c.Limit)
		}
		if c.OccurredFrom == nil || !c.OccurredFrom.Equal(*q.OccurredFrom) ||
			c.OccurredTo == nil || !c.OccurredTo.Equal(*q.OccurredTo) {
			t.Errorf("arm %s lost the occurred window", inc[0])
		}
		if !slices.Contains(c.ExcludeSourceTypes, model.SourceCall) ||
			!slices.Contains(c.ExcludeSourceTypes, model.SourceInsight) {
			t.Errorf("arm %s exclude set = %v", inc[0], c.ExcludeSourceTypes)
		}
		if !slices.Contains(c.ExcludeRetention, model.RetentionLow) {
			t.Errorf("arm %s exclude retention = %v", inc[0], c.ExcludeRetention)
		}
		if c.Query != q.Query {
			t.Errorf("arm %s query text changed", inc[0])
		}
	}
	slices.Sort(sources)
	want := []model.SourceType{model.SourceCalendar, model.SourceGmail, model.SourceNote, model.SourceSMS}
	if !slices.Equal(sources, want) { // call 은 질의가 제외했다
		t.Fatalf("arm sources = %v, want %v", sources, want)
	}

	// 실패한 gmail 보조 검색은 흡수되고, 전역 후보는 후보 풀에서 하나도
	// 빠지지 않는다(페이지 절단 전).
	for _, id := range []uuid.UUID{pfID(1), pfID(2), pfID(3)} {
		if !slices.Contains(trace.PoolIDs, id) {
			t.Errorf("global candidate %v lost from the pool", id)
		}
	}
	ids := pfIDs(got)
	// 필터를 무시한 저장소가 끼워 넣은 다른 소스 문서(14)는 note 보조 검색에서 걸러진다.
	if slices.Contains(trace.PoolIDs, pfID(14)) {
		t.Error("an arm admitted a document of another source")
	}
	// 3 은 전역·문자 보조 검색 양쪽에 있어 1위로 오른다.
	if ids[0] != pfID(3) {
		t.Errorf("doc in both lists should lead: got %v", ids)
	}
	if lanes := trace.LaneHits[pfID(10)]; !slices.Contains(lanes, LaneSourceStratify) {
		t.Errorf("calendar arm hit not traced: %v", lanes)
	}
	if len(got) != 5 {
		t.Errorf("page size = %d", len(got))
	}
}

func TestSourceStratify_AllArmsFailReturnsGlobalUnchanged(t *testing.T) {
	t.Parallel()
	st := armFixture()
	st.failFor = map[model.SourceType]bool{}
	for _, src := range stratifySources {
		st.failFor[src] = true
	}
	svc := NewService(st, disabledEmbedderForTrace{}).WithTuning(model.SearchTuning{})
	got, err := svc.Search(context.Background(), model.SearchQuery{Query: "q", Limit: 5,
		Tuning: model.SearchTuning{SourceStratifyK: 3}})
	if err != nil {
		t.Fatalf("arm failures must not fail the search: %v", err)
	}
	assertSameIDs(t, pfIDs(got), pfIDs(st.global))
	for i, r := range got {
		if r.Score != st.global[i].Score {
			t.Fatalf("scores changed although no arm contributed")
		}
	}
}

func TestStratifyArmSources(t *testing.T) {
	t.Parallel()
	sms := model.SourceSMS
	if got := stratifyArmSources(model.SearchQuery{}, 0); got != nil {
		t.Errorf("k=0: %v", got)
	}
	if got := stratifyArmSources(model.SearchQuery{SourceType: &sms}, 5); got != nil {
		t.Errorf("explicit include: %v", got)
	}
	// 레거시 별칭(call-log)도 정규화 후 call 을 제외한다.
	got := stratifyArmSources(model.SearchQuery{ExcludeSourceTypes: []model.SourceType{model.SourceCallLog, model.SourceGmail}}, 5)
	want := []model.SourceType{model.SourceCalendar, model.SourceSMS, model.SourceNote}
	if !slices.Equal(got, want) {
		t.Errorf("excluded: got %v, want %v", got, want)
	}
}

func TestMergeStratified(t *testing.T) {
	t.Parallel()
	global := []*model.SearchResult{
		pfDoc(1, model.SourceGmail, 0.9, nil),
		pfDoc(2, model.SourceSMS, math.NaN(), nil),
		pfDoc(3, model.SourceCall, 0.1, nil),
	}
	global[0].Evidence = []model.MatchedEvidence{{Score: 1}}
	arms := [][]*model.SearchResult{
		{pfDoc(3, model.SourceCall, 7, nil), pfDoc(4, model.SourceCall, 6, nil)},
		{pfDoc(5, model.SourceCalendar, 1, nil), pfDoc(5, model.SourceCalendar, 1, nil)}, // 목록 안 중복
		nil,
	}
	got := mergeStratified(global, arms)
	assertSameIDs(t, pfIDs(got), []uuid.UUID{pfID(3), pfID(1), pfID(5), pfID(2), pfID(4)})
	want3 := 1/(rrfK+3) + 1/(rrfK+1)
	if math.Abs(got[0].Score-want3) > 1e-12 {
		t.Errorf("doc in both lists score = %v, want %v", got[0].Score, want3)
	}
	if len(got[1].Evidence) != 1 {
		t.Error("global copy (with evidence) was not preferred")
	}
	if global[2].Score != 0.1 {
		t.Error("merge mutated the caller's results")
	}
	// 입력 순서를 바꿔도(목록 순서는 순위이므로 목록 간 순서만) 결과는 같다.
	swapped := mergeStratified(global, [][]*model.SearchResult{arms[2], arms[1], arms[0]})
	assertSameIDs(t, pfIDs(swapped), pfIDs(got))
	if len(mergeStratified(nil, nil)) != 0 {
		t.Error("empty merge produced results")
	}
}
