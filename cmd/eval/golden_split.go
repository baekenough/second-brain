package main

import (
	"sort"
	"time"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/timeutil"
)

// 상대 기간 질의의 날짜 분리.
//
// store.GoldenStore.ExportEvalPairs 는 질의 문구 하나를 쌍 하나로 묶고, 기준 시각으로
// 가장 이른 판정(또는 asked_at) 하나만 남긴다. "내일 일정 뭐야" 처럼 상대 기간이
// 든 질문을 서로 다른 날에 판정했다면 정답 문서는 날마다 다른 창에 있는데, 평가는
// 가장 이른 날의 창 하나로만 검색하므로 다른 날의 정답은 원천적으로 찾을 수 없다.
//
// --window=plan 이고 기준이 judged_at 또는 asked_at 일 때만, 결정론적 기간 파서가
// 기간 표현을 찾는 질문을 (문구, 기준 KST 날짜) 로 나눈다. 판정이 하루에 몰린 질문과
// 기간 표현이 없는 질문은 예전처럼 문구 하나로 묶이므로 쌍·label_hash 가 그대로다.

// goldenSplitApplies 는 날짜 분리가 의미 있는 설정인지 알린다. 창을 쓰지 않거나
// (--window=none) 모든 질의가 같은 기준 시각을 쓰면(--as-of) 나눠도 검색이 같다.
func goldenSplitApplies(windowMode, anchorKind string) bool {
	return windowMode == windowModePlan &&
		(anchorKind == windowAnchorJudgedAt || anchorKind == windowAnchorAskedAt)
}

// goldenPairsFromJudgments 는 판정 행을 평가 쌍으로 묶는다.
//
// 분리하지 않는 묶음은 ExportEvalPairs 와 같은 값을 만든다: 정답·오답 문서는 중복
// 없이 정렬, CreatedAt=MIN(judged_at), AskedAt=MIN(asked_at), 질의 id·출처는 사전순
// 최솟값. 분리된 묶음은 같은 규칙을 그 날짜의 판정에만 적용하고, 진단 덤프에서 서로
// 구별되도록 GoldenQueryID 뒤에 "@YYYY-MM-DD"(KST 기준 날짜)를 붙인다.
//
// 순서는 CreatedAt 내림차순, 문구 바이트 오름차순, 기준 날짜 오름차순이며 ID 는 그
// 순서의 1부터다 — 같은 입력이면 같은 ID 가 나온다.
func goldenPairsFromJudgments(rows []store.GoldenJudgmentRow, anchorKind string) []store.EvalPair {
	type group struct {
		text string
		date string // 분리된 묶음만; 분리하지 않으면 ""
		rows []store.GoldenJudgmentRow
	}
	byText := map[string][]store.GoldenJudgmentRow{}
	var texts []string
	for _, r := range rows {
		if _, seen := byText[r.QueryText]; !seen {
			texts = append(texts, r.QueryText)
		}
		byText[r.QueryText] = append(byText[r.QueryText], r)
	}
	sort.Strings(texts)

	var groups []group
	for _, text := range texts {
		rs := byText[text]
		byDate := map[string][]store.GoldenJudgmentRow{}
		var dates []string
		var earliest time.Time
		for _, r := range rs {
			a := judgmentAnchor(r, anchorKind)
			d := a.In(timeutil.KST()).Format("2006-01-02")
			if _, seen := byDate[d]; !seen {
				dates = append(dates, d)
			}
			byDate[d] = append(byDate[d], r)
			if earliest.IsZero() || a.Before(earliest) {
				earliest = a
			}
		}
		if len(dates) < 2 || !hasPeriodExpression(text, earliest) {
			groups = append(groups, group{text: text, rows: rs})
			continue
		}
		sort.Strings(dates)
		for _, d := range dates {
			groups = append(groups, group{text: text, date: d, rows: byDate[d]})
		}
	}

	pairs := make([]store.EvalPair, 0, len(groups))
	dates := make([]string, 0, len(groups))
	for _, g := range groups {
		p := aggregateJudgments(g.text, g.rows)
		if g.date != "" {
			p.GoldenQueryID += "@" + g.date
		}
		pairs = append(pairs, p)
		dates = append(dates, g.date)
	}
	idx := make([]int, len(pairs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		pa, pb := pairs[idx[a]], pairs[idx[b]]
		if !pa.CreatedAt.Equal(pb.CreatedAt) {
			return pa.CreatedAt.After(pb.CreatedAt)
		}
		if pa.Query != pb.Query {
			return pa.Query < pb.Query
		}
		return dates[idx[a]] < dates[idx[b]]
	})
	out := make([]store.EvalPair, len(pairs))
	for i, k := range idx {
		out[i] = pairs[k]
		out[i].ID = int64(i + 1)
	}
	return out
}

// judgmentAnchor 는 판정 행 하나의 기준 시각이다. asked_at 기준이면 그 질의 행의
// asked_at, 그 밖에는 판정 시각이다(windowAnchorFor 와 같은 출처).
func judgmentAnchor(r store.GoldenJudgmentRow, anchorKind string) time.Time {
	if anchorKind == windowAnchorAskedAt && !r.AskedAt.IsZero() {
		return r.AskedAt
	}
	return r.JudgedAt
}

// hasPeriodExpression 은 결정론적 기간 파서가 질문에서 기간을 찾는지 알린다. 평가가
// 창을 거는 바로 그 파서(planWindowResolver)다 — 창이 안 걸리는 질문을 나누면
// 검색은 같은데 label_hash 만 갈라진다.
func hasPeriodExpression(text string, anchor time.Time) bool {
	_, _, _, ok := intent.DeterministicWindow(text, anchor.In(timeutil.KST()))
	return ok
}

// aggregateJudgments 는 한 묶음의 판정을 ExportEvalPairs 와 같은 규칙으로 합친다.
func aggregateJudgments(text string, rows []store.GoldenJudgmentRow) store.EvalPair {
	p := store.EvalPair{Query: text, Source: "golden", RelevantDocIDs: []string{}, IrrelevantDocIDs: []string{}}
	pos, neg := map[string]bool{}, map[string]bool{}
	for i, r := range rows {
		switch r.Judgment {
		case "relevant":
			pos[r.DocumentID] = true
		case "irrelevant", "noise":
			neg[r.DocumentID] = true
		}
		if i == 0 || r.JudgedAt.Before(p.CreatedAt) {
			p.CreatedAt = r.JudgedAt
		}
		if i == 0 || r.AskedAt.Before(p.AskedAt) {
			p.AskedAt = r.AskedAt
		}
		if i == 0 || r.QueryID < p.GoldenQueryID {
			p.GoldenQueryID = r.QueryID
		}
		if i == 0 || r.QuerySource < p.GoldenQuerySource {
			p.GoldenQuerySource = r.QuerySource
		}
	}
	for id := range pos {
		p.RelevantDocIDs = append(p.RelevantDocIDs, id)
	}
	for id := range neg {
		p.IrrelevantDocIDs = append(p.IrrelevantDocIDs, id)
	}
	sort.Strings(p.RelevantDocIDs)
	sort.Strings(p.IrrelevantDocIDs)
	return p
}
