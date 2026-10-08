package search

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/timeutil"
	"github.com/google/uuid"
)

// 융합 이후(post-fusion) 변환. 전부 model.SearchTuning 의 기본 꺼짐 노브가
// 켜야만 동작하며, 꺼져 있으면 입력 슬라이스를 그대로 돌려준다 — 노브가 없는
// 배포의 결과는 이 파일이 생기기 전과 같다.
//
// Search 파이프라인 안의 적용 순서(search.go):
//
//	레인 융합(store·chunk·OpenSearch·chunk sparse)
//	→ [1] 소스별 보조 검색 RRF 합류      (SourceStratifyK, stratify.go)
//	→ 보존 페널티 → 최신성 감쇠 → recent 재정렬   (기존)
//	→ trace.FusedIDs 기록
//	→ [2a] 통화·문자 상대-날짜 접기        (CollapseContactDay)
//	→ 리랭크 / 합산                         (기존)
//	→ [5] MMR                              (MMRLambda)
//	→ [3] 시간창 날짜 버킷 다변화           (WindowBucketDiversify)
//	→ [2b] 접힌 문서 펼치기                (CollapseContactDay)
//	→ trace.PoolIDs 기록 → q.Limit 절단
//
// 순서의 이유:
//   - 보조 검색 합류는 "후보 풀 멤버십" 을 바꾸므로 융합 단계에 속한다.
//     보존 페널티·최신성 감쇠가 합류한 문서에도 똑같이 한 번만 적용되도록 그
//     앞에 둔다.
//   - 접기는 리랭크 앞이어야 리랭커 예산(후보 수)과 상위 순위 자리를 같은
//     상대·같은 날 문서들이 독식하지 않는다. FusedIDs 기록 뒤에 두어 진단의
//     융합 기준선에는 모든 후보가 남는다.
//   - MMR 은 리랭크·합산이 정한 관련도를 입력으로 쓰므로 그 뒤다.
//   - 날짜 버킷 다변화는 "첫 바퀴는 날짜마다 하나" 라는 구조적 재배치라 가장
//     마지막 재정렬이어야 한다. MMR 뒤에 두어도 MMR 이 정한 순서를 날짜별
//     대표 선택의 순위로 그대로 쓴다.
//   - 펼치기는 최종 순서가 다 정해진 뒤 대표 바로 뒤에 같은 그룹을 끼운다.
//     같은 그룹은 같은 날이므로 날짜 버킷 구조를 깨지 않는다.
//
// 공통 규약:
//   - 입력 슬라이스의 순서가 곧 "현재 순위" 다. 파이프라인에서 이 변환들에
//     들어오는 목록은 sortByScore(총순서) 또는 리랭크 순위로 이미 정렬돼 있다.
//     맵 순회 순서에 의존하는 곳은 없다 — 같은 입력이면 항상 같은 출력이다.
//   - 재배치가 하나라도 일어나면 응답 Score 는 최종 순서를 따라 비증가가
//     되도록 내려 깎인다(clampNonIncreasing). 노브가 꺼져 있으면 점수는 그대로다.
//   - 어떤 후보도 버리지 않는다. 위치만 바꾼다(보조 검색 합류는 후보를 더할
//     뿐이다).
//   - Sort="recent" 질의에서는 순서 변환(접기·MMR·날짜 버킷)을 하지 않는다.
//     그 질의의 순서는 시간이 정하고, 관련도 다변화는 그 계약을 깨뜨린다.
//   - 질의·문서 내용은 어떤 로그에도 싣지 않는다. 그룹 키(상대 해시 포함)도
//     로그·trace 에 남기지 않는다.

// MMRRankK 는 MMR 순위 기반 관련도 (k+1)/(k+rank) 의 k 다. RRF 의 60 이
// 아니라 10 을 쓴다: k=60 이면 창(30건) 안 관련도가 1.0→0.68 로만 줄어
// 코사인 유사도(0..1) 벌점에 비해 순위 신호가 거의 평평해지고, 10 이면
// 1.0→0.28 로 상위와 꼬리가 분명히 구분된다. 실행 프로필에도 기록된다.
const MMRRankK = 10

// MMRWindow 는 MMR 이 재배치하는 상위 후보 수다. 이 밖의 순서는 건드리지
// 않는다. 실행 프로필에도 기록된다(cmd/eval).
const MMRWindow = 30

// collapsedGroup 은 접힌 그룹 하나다. rep 는 순위에 남은 대표, members 는
// 순위에서 잠시 빠진 같은 그룹 문서들(점수 순)이다.
type collapsedGroup struct {
	rep     uuid.UUID
	members []*model.SearchResult
}

// collapsedSet 은 접기 결과다. groups 는 대표가 처음 나타난 순위 순서라
// 결정론적이고, byRep 은 대표 ID 로 groups 를 찾는 색인이다. pre 는 접기
// 직전의 순위(입력 순서)로, 펼침 상한을 넘은 멤버를 원래 자리 근처로 되돌릴
// 때 쓴다(expandCollapsed).
type collapsedSet struct {
	groups []collapsedGroup
	byRep  map[uuid.UUID]int
	max    int
	pre    []*model.SearchResult
}

// collapseForRanking 은 노브·질의 조건을 확인한 뒤 collapseContactDay 를
// 적용한다. 꺼져 있으면 입력을 그대로, 두 번째 값은 nil 이다.
func collapseForRanking(q model.SearchQuery, results []*model.SearchResult, tune model.SearchTuning) ([]*model.SearchResult, *collapsedSet) {
	if !tune.CollapseContactDay || q.SortsByRecency() || len(results) < 2 {
		return results, nil
	}
	return collapseContactDay(results, tune.CollapseExpandMax)
}

// collapseContactDay 는 통화·문자 문서를 (소스, 상대, KST 날짜) 그룹으로 접는다
// (Elasticsearch collapse / LlamaIndex AutoMerging 과 같은 발상).
//
// 의미:
//   - 그룹 키가 있는 문서(contactDayKey)끼리만 접는다. 키가 없는 문서(다른
//     소스, occurred_at 없음, 상대 식별자 없음)는 저마다 혼자인 그룹이라
//     접히지 않는다.
//   - 대표는 그룹에서 resultBefore 기준 최상위(점수 내림차순 → 사건 시각
//     최신 → ID)다. 대표는 입력 안의 자기 위치를 그대로 지키고, 나머지
//     멤버는 순위 목록에서 빠져 collapsedSet 에 점수 순으로 보관된다.
//   - 그 결과 같은 상대·같은 날 문서 여러 건이 차지하던 자리가 다른 후보에게
//     열린다. 리랭커도 대표만 본다.
//   - 문서는 하나도 버려지지 않는다. expandCollapsed 가 최종 순서가 정해진
//     뒤 모든 멤버를 다시 넣는다.
//
// 접을 그룹이 하나도 없으면 입력을 그대로 돌려주고 두 번째 값은 nil 이다.
func collapseContactDay(results []*model.SearchResult, expandMax int) ([]*model.SearchResult, *collapsedSet) {
	keys := make([]string, len(results))
	best := make(map[string]int, len(results)) // 그룹 키 → 대표의 입력 인덱스
	sizes := make(map[string]int, len(results))
	for i, r := range results {
		if r == nil {
			continue
		}
		k, ok := contactDayKey(r)
		if !ok {
			continue
		}
		keys[i] = k
		sizes[k]++
		if j, seen := best[k]; !seen || resultBefore(r, results[j]) {
			best[k] = i
		}
	}
	collapsible := false
	for _, n := range sizes {
		if n > 1 {
			collapsible = true
			break
		}
	}
	if !collapsible {
		return results, nil
	}

	set := &collapsedSet{byRep: make(map[uuid.UUID]int), max: expandMax, pre: slices.Clone(results)}
	groupIdx := make(map[string]int)
	// 1회차: 대표가 나타나는 순위 순서대로 그룹을 만든다(결정론적 순서).
	for i, r := range results {
		k := keys[i]
		if k == "" || sizes[k] < 2 || best[k] != i {
			continue
		}
		if _, dup := set.byRep[r.ID]; dup {
			// 같은 ID 가 두 그룹의 대표가 되는 비정상 입력. 두 번째 그룹은
			// 접지 않는다 — 그 멤버는 아래 2회차에서 제자리에 남는다.
			continue
		}
		groupIdx[k] = len(set.groups)
		set.byRep[r.ID] = len(set.groups)
		set.groups = append(set.groups, collapsedGroup{rep: r.ID})
	}
	// 2회차: 그룹이 만들어진 키의 비대표 멤버만 순위에서 빼 그룹에 모은다.
	// 나머지는 전부 입력 순서 그대로 순위에 남는다.
	ranked := make([]*model.SearchResult, 0, len(results))
	for i, r := range results {
		k := keys[i]
		gi, grouped := groupIdx[k]
		if k == "" || !grouped || best[k] == i {
			ranked = append(ranked, r)
			continue
		}
		set.groups[gi].members = append(set.groups[gi].members, r)
	}
	for gi := range set.groups {
		sortByScore(set.groups[gi].members)
	}
	return ranked, set
}

// expandCollapsed 는 접힌 멤버를 다시 펼친다. 최종 순위(ranked)의 각 항목을
// "블록" 의 머리로 보고 다음 규칙으로 멤버를 끼운다.
//
//   - 펼침: 대표의 블록에는 대표 바로 뒤에 그 그룹 멤버를 점수 순으로 최대
//     set.max 건 붙인다.
//   - 초과분(set.max 를 넘은 멤버, 대표가 최종 목록에 없는 그룹의 멤버 전부):
//     접기 직전 순위(set.pre)에서 그 멤버 바로 위에 있던 가장 가까운 블록
//     머리(접히지 않고 순위에 남았던 항목 — 대표 또는 그룹 밖 문서)를 앵커로
//     삼아, 최종 순위에서 앵커 블록 바로 뒤에 pre 순서대로 붙인다. 따라서
//     리랭크가 순서를 바꾸지 않았다면 초과분과 그룹 밖 문서의 상대 순서는
//     접기 전과 같다 — 초과분이 목록 꼬리로 밀려 페이지에서 사라지지 않는다.
//     리랭크가 순서를 바꿨다면 초과분은 원래 자기 바로 위에 있던 문서를
//     따라간다. 앵커가 없으면(입력이 점수 순이 아닐 때만 생긴다) 맨 뒤에 붙인다.
//   - 점수: 끼워 넣은 문서(펼침·초과분)는 얕은 복사본의 Score 를 바로 앞에
//     놓인 문서의 점수로 맞춘다(동점). 대표는 리랭커 점수, 멤버는 융합 점수라
//     척도가 달라서, 자기 점수를 그대로 두면 응답의 점수가 순서와 어긋난다.
//     블록 머리들의 점수가 비증가면 결과 전체도 비증가다.
//
// 대표 ID 가 목록에 두 번 나오면 첫 번째에서만 펼친다. 어떤 후보도 버리지 않는다.
func expandCollapsed(ranked []*model.SearchResult, set *collapsedSet) []*model.SearchResult {
	if set == nil || len(set.groups) == 0 {
		return ranked
	}

	// 1) 블록 구성: 머리 + (대표면) 펼침 멤버. 펼치지 못한 멤버는 초과분.
	type block struct {
		items    []*model.SearchResult
		overflow []*model.SearchResult
	}
	blocks := make([]block, 0, len(ranked))
	headBlock := make(map[uuid.UUID]int, len(ranked))
	expanded := make([]bool, len(set.groups))
	overflow := make(map[*model.SearchResult]bool)
	for _, r := range ranked {
		b := block{items: []*model.SearchResult{r}}
		if r != nil {
			if _, seen := headBlock[r.ID]; !seen {
				headBlock[r.ID] = len(blocks)
			}
			if gi, ok := set.byRep[r.ID]; ok && !expanded[gi] {
				expanded[gi] = true
				members := set.groups[gi].members
				n := min(max(set.max, 0), len(members))
				b.items = append(b.items, members[:n]...)
				for _, m := range members[n:] {
					overflow[m] = true
				}
			}
		}
		blocks = append(blocks, b)
	}
	for gi, g := range set.groups {
		if !expanded[gi] {
			for _, m := range g.members {
				overflow[m] = true
			}
		}
	}

	// 2) 초과분을 접기 직전 순위의 앵커 블록에 붙인다(pre 순서 유지).
	var orphans []*model.SearchResult
	for i, m := range set.pre {
		if !overflow[m] {
			continue
		}
		anchor := -1
		for j := i - 1; j >= 0 && anchor < 0; j-- {
			p := set.pre[j]
			if p == nil || overflow[p] {
				continue
			}
			// 리랭크는 얕은 복사본을 돌려주므로 포인터가 아니라 ID 로 찾는다.
			// 펼친 멤버의 ID 는 블록 머리가 아니라 여기서 걸리지 않는다.
			if bi, ok := headBlock[p.ID]; ok {
				anchor = bi
			}
		}
		if anchor < 0 {
			orphans = append(orphans, m)
			continue
		}
		blocks[anchor].overflow = append(blocks[anchor].overflow, m)
	}

	// 3) 내보내기. 끼워 넣은 문서는 바로 앞 문서의 점수를 이어받는다.
	out := make([]*model.SearchResult, 0, len(set.pre))
	inherit := func(r *model.SearchResult) {
		if len(out) > 0 && out[len(out)-1] != nil && !isBadScore(out[len(out)-1].Score) {
			cp := *r // 얕은 복사 — 호출자의 결과를 변형하지 않는다
			cp.Score = out[len(out)-1].Score
			r = &cp
		}
		out = append(out, r)
	}
	for _, b := range blocks {
		out = append(out, b.items[0])
		for _, r := range b.items[1:] {
			inherit(r)
		}
		for _, r := range b.overflow {
			inherit(r)
		}
	}
	for _, r := range orphans {
		inherit(r)
	}
	return out
}

func isBadScore(f float64) bool { return math.IsNaN(f) || math.IsInf(f, 0) }

// clampNonIncreasing 은 응답의 Score 가 최종 순서를 따라 비증가가 되도록,
// 앞 문서보다 점수가 높은 문서의 점수를 앞 문서 점수로 낮춘다(얕은 복사).
// 순서는 바꾸지 않는다. 융합 이후 재배치(MMR·날짜 버킷·펼치기·리랭크 입력
// 상한 뒤의 꼬리)가 실제로 켜졌을 때만 부른다 — 재배치는 점수를 바꾸지 않고
// 순서만 바꾸므로, 그대로 두면 응답의 점수가 순서와 어긋난다. 점수를 올리지는
// 않는다(승격된 문서에 없던 관련도를 붙이지 않는다). NaN·무한대 점수는 그대로
// 두고 기준선 갱신에도 쓰지 않는다.
func clampNonIncreasing(results []*model.SearchResult) []*model.SearchResult {
	out := slices.Clone(results)
	floor := math.Inf(1)
	for i, r := range out {
		if r == nil || isBadScore(r.Score) {
			continue
		}
		if r.Score > floor {
			cp := *r
			cp.Score = floor
			out[i] = &cp
			continue
		}
		floor = r.Score
	}
	return out
}

// contactDayKey 는 접기 그룹 키 "(소스)|(상대)|(KST 날짜)" 를 만든다. 문자·
// 통화가 아니거나, occurred_at 이 없거나, 상대 식별자가 없으면 false 다.
//
// 이 키는 상대 해시를 담으므로 로그·trace·덤프에 절대 싣지 않는다.
func contactDayKey(r *model.SearchResult) (string, bool) {
	st := model.NormalizeSourceType(r.SourceType)
	if st != model.SourceSMS && st != model.SourceCall {
		return "", false
	}
	if r.OccurredAt == nil {
		return "", false
	}
	who := contactKey(r)
	if who == "" {
		return "", false
	}
	day := r.OccurredAt.In(timeutil.KST()).Format(time.DateOnly)
	return string(st) + "|" + who + "|" + day, true
}

// contactKey 는 상대 식별자를 우선순위대로 찾는다.
//
//  1. metadata.number_hash (녹음 수집 경로가 남긴다)
//  2. metadata.thread_id
//  3. SourceID 의 상대 해시 구간 — smsmap 이 "sms:{ms}:{hash}:{dir}",
//     "call-log:{ms}:{hash}:{dur}" 형식으로 남긴다. 문자·통화 대부분은
//     메타데이터에 number_hash 가 없으므로(번호 해시는 SourceID 에만 있다)
//     이 대체 경로가 없으면 노브가 사실상 동작하지 않는다.
//
// 연락처 표시 이름은 쓰지 않는다 — 같은 이름의 다른 사람을 한 그룹으로
// 묶을 수 있다. 접두어(h:/t:/s:)로 출처가 다른 값이 우연히 같아도 섞이지
// 않게 한다.
func contactKey(r *model.SearchResult) string {
	if h := metaScalar(r.Metadata, "number_hash"); h != "" {
		return "h:" + h
	}
	if t := metaScalar(r.Metadata, "thread_id"); t != "" {
		return "t:" + t
	}
	if h := sourceIDContactHash(r.SourceID); h != "" {
		return "s:" + h
	}
	return ""
}

func sourceIDContactHash(sourceID string) string {
	parts := strings.Split(sourceID, ":")
	if len(parts) < 3 || (parts[0] != "sms" && parts[0] != "call-log") {
		return ""
	}
	return strings.TrimSpace(parts[2])
}

// metaScalar 는 메타데이터 값을 문자열로 읽는다. JSON 디코딩 경로에 따라
// 숫자가 float64·json.Number 로 올 수 있어 셋 다 받는다. 그 외 형은 빈 값이다.
func metaScalar(meta map[string]any, key string) string {
	switch v := meta[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return ""
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	default:
		return ""
	}
}

// applyPostRerank 는 리랭크(합산) 뒤의 변환을 파일 머리 주석의 순서대로
// 적용한다: MMR → 날짜 버킷 다변화 → 접힌 문서 펼치기 → 점수 비증가 보정.
// rerankTail 은 리랭크 입력 상한(rerankInputCap) 때문에 리랭크되지 않고 뒤에
// 붙은 꼬리가 있었는지다. 어느 재배치도 일어나지 않았으면 입력을 그대로
// 돌려준다(점수 보정도 하지 않는다).
func applyPostRerank(q model.SearchQuery, results []*model.SearchResult, collapsed *collapsedSet, tune model.SearchTuning, rerankTail bool) []*model.SearchResult {
	reordered := rerankTail
	if !q.SortsByRecency() {
		if tune.MMRLambda > 0 {
			results = applyMMR(results, tune.MMRLambda)
			reordered = true
		}
		if tune.WindowBucketDiversify && windowSpansMultipleDays(q) {
			results = diversifyByDay(results)
			reordered = true
		}
	}
	if collapsed != nil {
		results = expandCollapsed(results, collapsed)
		reordered = true
	}
	if !reordered {
		return results
	}
	return clampNonIncreasing(results)
}

// windowSpansMultipleDays 는 질의에 KST 로 하루를 넘는 occurred 시간창이
// 있는지 알린다. 창은 [From, To) 반열림 구간이다. 한쪽 경계만 있으면 끝이
// 열린 창이므로 하루를 넘는 것으로 본다. To<=From 인 빈 창은 false.
func windowSpansMultipleDays(q model.SearchQuery) bool {
	from, to := q.OccurredFrom, q.OccurredTo
	switch {
	case from == nil && to == nil:
		return false
	case from == nil || to == nil:
		return true
	case !to.After(*from):
		return false
	}
	kst := timeutil.KST()
	first := from.In(kst).Format(time.DateOnly)
	last := to.Add(-time.Nanosecond).In(kst).Format(time.DateOnly)
	return first != last
}

// diversifyByDay 는 날짜(KST) 버킷 다변화다(Hindsight 의 시간 arm).
//
// 첫 바퀴에서 날짜마다 가장 순위가 높은 문서 하나씩을 현재 순위 순서대로
// 세우고, 나머지는 현재 순위 순서 그대로 그 뒤에 둔다. 순수 재배치다.
// occurred_at 이 없는 문서는 날짜를 대표할 수 없으므로 첫 바퀴에서 빠져
// 나머지 쪽에 남는다. 한 날짜에만 문서가 몰려 있으면 순서가 바뀌지 않는다.
func diversifyByDay(results []*model.SearchResult) []*model.SearchResult {
	if len(results) < 2 {
		return results
	}
	kst := timeutil.KST()
	seen := make(map[string]bool)
	first := make([]*model.SearchResult, 0, len(results))
	rest := make([]*model.SearchResult, 0, len(results))
	for _, r := range results {
		if r == nil || r.OccurredAt == nil {
			rest = append(rest, r)
			continue
		}
		day := r.OccurredAt.In(kst).Format(time.DateOnly)
		if seen[day] {
			rest = append(rest, r)
			continue
		}
		seen[day] = true
		first = append(first, r)
	}
	return append(first, rest...)
}

// applyMMR 는 상위 MMRWindow 건에 Maximal Marginal Relevance 를 적용한다
// (Graphiti / LangChain 의 MMR 재정렬).
//
//	mmr(d) = λ·rel(d) − (1−λ)·max_{s∈선택됨, 같은 소스} cos(d, s)
//
// 규칙:
//   - rel 은 점수가 아니라 현재 순위에서 나온다:
//     rel = (MMRRankK+1)/(MMRRankK+rank), rank 는 창 안 1-기반 위치라 1위가
//     1.0 이다. 원점수를 쓰지 않는 이유: 리랭커가 상위 N건만 돌려주면
//     applyRerank 는 나머지를 점수 0 으로 채워 뒤에 붙이는데, 리랭커 점수가
//     음수(로짓)이면 min-max 정규화에서 그 0점 꼬리가 최고 관련도가 되어
//     위로 올라온다. 순위 기반이면 점수 척도(리랭커 확률·로짓·RRF 합)와
//     무관하게 관련도는 항상 순위 단조 감소이고, NaN 점수도 문제되지 않는다.
//     동점은 현재 순위가 앞선 쪽이 이긴다.
//   - 유사도 벌점은 같은 source_type(정규화 후) 문서 사이에만 준다. 통화와
//     메일이 비슷하다는 이유로 서로를 밀어내는 것은 다양화가 아니다. 같은
//     소스로 이미 뽑힌 문서가 없으면 벌점은 0 이다.
//   - 문서 임베딩(Document.Embedding)이 없거나 쓸 수 없는(차원 0, NaN, 영벡터)
//     문서는 자기 자리를 그대로 지킨다. 재배치는 임베딩이 있는 문서들이
//     차지하던 자리 안에서만 일어난다. 차원이 다른 두 벡터의 유사도는 0 이다.
//   - λ=1 이면 순수 관련도(= 현재 순위)라 순서가 바뀌지 않는다.
//
// 창 밖 문서와 결과 크기는 바뀌지 않는다. 점수(Score)는 여기서 바꾸지 않고,
// applyPostRerank 가 마지막에 비증가로 보정한다(clampNonIncreasing).
func applyMMR(results []*model.SearchResult, lambda float64) []*model.SearchResult {
	if lambda <= 0 || lambda > 1 || math.IsNaN(lambda) || len(results) < 2 {
		return results
	}
	n := min(len(results), MMRWindow)

	type cand struct {
		r    *model.SearchResult
		vec  []float32
		norm float64
		st   model.SourceType
		rel  float64
	}
	var slots []int
	var cands []*cand
	for i := 0; i < n; i++ {
		r := results[i]
		if r == nil {
			continue
		}
		norm, ok := vectorNorm(r.Embedding)
		if !ok {
			continue
		}
		slots = append(slots, i)
		cands = append(cands, &cand{r: r, vec: r.Embedding, norm: norm, st: model.NormalizeSourceType(r.SourceType)})
	}
	if len(cands) < 2 {
		return results
	}

	for k, c := range cands {
		rank := float64(slots[k] + 1) // 창 안 위치(임베딩 없는 문서도 순위를 차지한다)
		c.rel = (MMRRankK + 1) / (MMRRankK + rank)
	}

	maxSim := make([]float64, len(cands))
	hasSame := make([]bool, len(cands))
	picked := make([]bool, len(cands))
	order := make([]*model.SearchResult, 0, len(cands))
	for step := 0; step < len(cands); step++ {
		best, bestVal := -1, math.Inf(-1)
		for i, c := range cands {
			if picked[i] {
				continue
			}
			v := lambda * c.rel
			if hasSame[i] {
				v -= (1 - lambda) * maxSim[i]
			}
			if best < 0 || v > bestVal {
				best, bestVal = i, v
			}
		}
		picked[best] = true
		order = append(order, cands[best].r)
		b := cands[best]
		for i, c := range cands {
			if picked[i] || c.st != b.st {
				continue
			}
			sim := cosine(b.vec, c.vec, b.norm, c.norm)
			if !hasSame[i] || sim > maxSim[i] {
				maxSim[i], hasSame[i] = sim, true
			}
		}
	}

	out := make([]*model.SearchResult, len(results))
	copy(out, results)
	for k, slot := range slots {
		out[slot] = order[k]
	}
	return out
}

// vectorNorm 은 L2 노름을 돌려준다. 빈 벡터·NaN/무한대 성분·영벡터면 false.
func vectorNorm(v []float32) (float64, bool) {
	if len(v) == 0 {
		return 0, false
	}
	var sum float64
	for _, x := range v {
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		sum += f * f
	}
	if sum == 0 || math.IsInf(sum, 0) {
		return 0, false
	}
	return math.Sqrt(sum), true
}

// cosine 은 미리 계산한 노름으로 코사인 유사도를 구한다. 차원이 다르면 0.
func cosine(a, b []float32, na, nb float64) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot / (na * nb)
}
