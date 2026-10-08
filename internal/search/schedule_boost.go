package search

import (
	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/model"
)

// applyScheduleIntentBoost 는 일정 의도 질문에서 캘린더 후보를 끌어올린다
// (SEARCH_SCHEDULE_INTENT_BOOST, 기본 꺼짐).
//
// 겨냥하는 실패: 결정론적 계획이 캘린더로 좁히지 않은 일정 질문(일정·스케줄·
// 캘린더·약속 단어가 없거나 계획이 거절함)에서 통화·메일이 관련 캘린더 문서
// 위를 차지한다. 하드 필터 대신 순위 가산이라, 틀린 판정의 비용은 순서
// 몇 칸이지 정답 소멸이 아니다.
//
// 적용 조건(하나라도 아니면 입력을 그대로 돌려준다):
//   - tune.ScheduleIntentBoost > 0
//   - 질의에 소스 포함 집합이 없다(이미 좁혀진 질의는 손대지 않는다)
//   - Sort 가 recent 가 아니다(그 질의의 순서는 시간이 정한다)
//   - intent.HasScheduleIntent(질문 원문)
//
// 동작: 캘린더 후보(정규화한 source_type)의 양수·유한 점수에 (1+boost) 를
// 곱한 얕은 복사본을 만들고 sortByScore 로 다시 정렬한다. 후보를 더하거나
// 빼지 않는다. 0 이하·NaN 점수는 곱하면 오히려 내려가거나 의미가 없으므로
// 건드리지 않는다 — 그래서 움직이는 것은 캘린더 문서가 위로 올라가는 것뿐이다.
func applyScheduleIntentBoost(q model.SearchQuery, results []*model.SearchResult, tune model.SearchTuning) []*model.SearchResult {
	if tune.ScheduleIntentBoost <= 0 || len(results) < 2 || q.SortsByRecency() ||
		len(q.IncludeSourceTypes()) > 0 || !intent.HasScheduleIntent(q.Query) {
		return results
	}
	out := make([]*model.SearchResult, len(results))
	changed := false
	for i, r := range results {
		out[i] = r
		if r == nil || model.NormalizeSourceType(r.SourceType) != model.SourceCalendar ||
			isBadScore(r.Score) || r.Score <= 0 {
			continue
		}
		cp := *r // 얕은 복사 — 레인 결과를 변형하지 않는다
		cp.Score *= 1 + tune.ScheduleIntentBoost
		out[i] = &cp
		changed = true
	}
	if !changed {
		return results
	}
	sortByScore(out)
	return out
}
