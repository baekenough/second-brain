package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/baekenough/second-brain/internal/evaldump"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// dumpFileMode 는 진단 파일의 퍼미션이다. 질의 문구도 본문도 담지 않지만
// 문서 ID·시각·소스 조합만으로도 코퍼스 구조가 드러나므로 소유자 전용으로 쓴다.
const dumpFileMode os.FileMode = 0o600

// queryDiagnostics 는 질의 한 건의 진단 행이다. NDCG 한 숫자가 답하지 못하는
// 질문 — "이 정답 문서는 회수조차 안 된 건가, 회수됐는데 10위 밖인가,
// 리랭커가 밀어낸 건가" — 를 구분하기 위한 최소 증거다.
//
// 질의 텍스트·문서 제목·본문은 어떤 필드에도 담지 않는다. 이 파일은 평가
// 산출물로 남고 돌아다니며, 원문이 한 번 섞이면 전부 개인정보 취급이 된다.
// 질의를 지목해야 할 때는 golden_queries.id 나 질의 텍스트의 해시 접두사만
// 쓰고, 길이(query_len)로 대략의 형태만 남긴다.
type queryDiagnostics struct {
	// Kind is empty on a v1 dump (the shape that shipped before #269) and
	// "query" on v2, set by attachQueryMetrics. internal/evaldump.Read uses
	// it, together with line 1, to tell the two formats apart.
	Kind string `json:"kind"`

	QueryID     string `json:"query_id"`
	QuerySource string `json:"query_source"`
	QueryLen    int    `json:"query_len"`

	// WindowApplied 는 --window=plan 에서 실제로 검색에 걸린 시간창이다.
	// 모드가 none 이거나 기간 표현이 없어 파서가 매칭하지 못하면 null.
	WindowApplied *dumpWindow `json:"window_applied"`

	RelevantDocIDs []string    `json:"relevant_doc_ids"`
	RelevantDocs   []dumpLabel `json:"relevant_docs"`

	Top10IDs         []string `json:"top10_ids"`
	Top10SourceTypes []string `json:"top10_source_types"`
	NegativeInTop10  int      `json:"negative_ids_in_top10"`

	// OverfetchPoolSize 는 페이지 크기로 자르기 직전 후보 풀의 크기다.
	// dumpLabel.InOverfetchPool 을 읽을 때의 분모에 해당한다.
	OverfetchPoolSize int `json:"overfetch_pool_size"`

	SearchFailed    bool `json:"search_failed"`
	RerankRequested bool `json:"rerank_requested"`
	RerankAttempted bool `json:"rerank_attempted"`
	RerankFailed    bool `json:"rerank_failed"`

	// --- Dump v2 (#269): filled by attachQueryMetrics, never by
	// buildDiagnostics itself. NDCG10/Recall10 stay nil for a query with no
	// positive labels — 0 and "not scoreable" are different facts, and a
	// group-level comparator must not average the two together.
	LatencyMs *float64 `json:"latency_ms"`
	NDCG10    *float64 `json:"ndcg10"`
	Recall10  *float64 `json:"recall10"`
	// FP10 mirrors NegativeInTop10 under the dump-v2 metric name (issue #269
	// design: fp10 is the same count, not a re-derivation).
	FP10 *int `json:"fp10"`
}

// dumpWindow 는 적용된 시간창의 반열린 구간 [from, to) 이다.
type dumpWindow struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// dumpLabel 은 정답 라벨 문서 한 건이 검색 파이프라인의 어디까지 올라왔는지를
// 기록한다.
type dumpLabel struct {
	DocID string `json:"doc_id"`
	// FinalRank 는 호출자에게 돌아간 결과 안에서의 1-기반 순위. 결과에
	// 없으면 null 이며, 이는 0 위와 전혀 다른 뜻이다.
	FinalRank *int `json:"final_rank"`
	// PreRerankRank 는 리랭커에 넘기기 직전 후보 순서에서의 1-기반 순위.
	// 리랭커를 호출하지 않았거나 그 후보에 없었으면 null.
	PreRerankRank *int `json:"pre_rerank_rank"`
	// FusedRank 는 리랭크 합산이 개입하기 직전의 융합 순위(1-기반)다.
	// 리랭크를 끈 실행에서도 채워지므로, 같은 질의의 rerank on/off 실행을
	// 이 값 하나로 나란히 놓을 수 있다. PreRerankRank 는 리랭크를 실제로
	// 호출했을 때만 채워진다는 점이 다르다. 후보에 없었으면 null.
	FusedRank *int `json:"fused_rank"`
	// InOverfetchPool 은 페이지 크기로 잘리기 전 후보 풀에 있었는지 여부다.
	// false 면 순위 문제가 아니라 회수(recall) 문제다.
	InOverfetchPool bool `json:"in_overfetch_pool"`
	// LanesHit 은 이 문서를 후보로 올린 레인 이름들이다(빈 배열 = 어느
	// 레인도 올리지 못함).
	LanesHit []string `json:"lanes_hit"`

	SourceType string `json:"source_type"`
	Retention  string `json:"retention"`
	// OccurredAt 은 날짜까지만 남긴다(YYYY-MM-DD). 시각까지 적으면 특정
	// 통화·메시지를 지목하는 단서가 된다. 값이 없으면 빈 문자열.
	OccurredAt string `json:"occurred_at"`
}

// buildDiagnostics 는 검색 결과와 추적 기록으로 진단 행 하나를 만든다.
// 문서 메타데이터(source_type/retention/occurred_at)는 아직 채우지 않는다 —
// enrichDiagnostics 가 DB 를 한 번만 읽어 일괄로 채운다.
func buildDiagnostics(pair store.EvalPair, window *dumpWindow, results []*model.SearchResult,
	trace *search.SearchTrace, failed bool, negative map[string]bool) queryDiagnostics {
	diag := queryDiagnostics{
		QueryID:        diagnosticQueryID(pair),
		QuerySource:    diagnosticQuerySource(pair),
		QueryLen:       len([]rune(pair.Query)),
		WindowApplied:  window,
		RelevantDocIDs: append([]string{}, pair.RelevantDocIDs...),
		SearchFailed:   failed,
	}
	if diag.RelevantDocIDs == nil {
		diag.RelevantDocIDs = []string{}
	}
	if trace != nil {
		diag.RerankRequested = trace.RerankRequested
		diag.RerankAttempted = trace.RerankAttempted
		diag.RerankFailed = trace.RerankFailed
		diag.OverfetchPoolSize = len(trace.PoolIDs)
	}

	finalRank := map[string]int{}
	diag.Top10IDs = make([]string, 0, len(results))
	diag.Top10SourceTypes = make([]string, 0, len(results))
	for i, r := range results {
		id := r.ID.String()
		finalRank[id] = i + 1
		diag.Top10IDs = append(diag.Top10IDs, id)
		diag.Top10SourceTypes = append(diag.Top10SourceTypes, string(r.SourceType))
		if negative[id] {
			diag.NegativeInTop10++
		}
	}

	poolRank := rankIndex(traceIDs(trace, func(t *search.SearchTrace) []uuid.UUID { return t.PoolIDs }))
	preRank := rankIndex(traceIDs(trace, func(t *search.SearchTrace) []uuid.UUID { return t.PreRerankIDs }))
	fusedRank := rankIndex(traceIDs(trace, func(t *search.SearchTrace) []uuid.UUID { return t.FusedIDs }))

	diag.RelevantDocs = make([]dumpLabel, 0, len(pair.RelevantDocIDs))
	for _, id := range pair.RelevantDocIDs {
		label := dumpLabel{DocID: id, LanesHit: []string{}}
		if rank, ok := finalRank[id]; ok {
			label.FinalRank = &rank
		}
		if rank, ok := preRank[id]; ok {
			label.PreRerankRank = &rank
		}
		if rank, ok := fusedRank[id]; ok {
			label.FusedRank = &rank
		}
		_, label.InOverfetchPool = poolRank[id]
		if trace != nil {
			if parsed, err := uuid.Parse(id); err == nil {
				if lanes := trace.LaneHits[parsed]; len(lanes) > 0 {
					label.LanesHit = append([]string{}, lanes...)
				}
			}
		}
		diag.RelevantDocs = append(diag.RelevantDocs, label)
	}
	return diag
}

// attachQueryMetrics 는 dump v2 를 위해 질의별 지연시간과 NDCG10/Recall10/FP10 을
// 채운다. diags 와 latencies 는 evaluatePairs 가 만든 순서 그대로다(pairs 와
// 같은 순서, evaluation.Latencies 도 마찬가지).
//
// NDCG10 은 search.NDCGK(top10, positives, 10) — Aggregate 가 macro-average 를
// 낼 때 쓰는 바로 그 함수 — 로 계산한다. 따로 구현하면 언젠가 두 계산이
// 갈라지고, 그 어긋남은 집계 점수와 덤프 점수가 서로 다른 이야기를 하는 채로
// 발견되지 않는다. diags[i].Top10IDs 와 RelevantDocIDs 는 buildDiagnostics 가
// 이미 채운 값이므로 evaluatePairs 쪽 인자를 새로 받을 필요가 없다.
func attachQueryMetrics(diags []queryDiagnostics, latencies []float64) {
	for i := range diags {
		diags[i].Kind = "query"

		fp := diags[i].NegativeInTop10
		diags[i].FP10 = &fp

		if i < len(latencies) {
			ms := latencies[i]
			diags[i].LatencyMs = &ms
		}

		if len(diags[i].RelevantDocIDs) == 0 {
			// 정답 라벨이 없는 질의(부정 전용 질의 등) — NDCG/Recall 은
			// 잴 대상이 없다. 0 으로 채우면 "재봤더니 0 점"과 구분이 안 된다.
			continue
		}
		relevant := make(map[string]bool, len(diags[i].RelevantDocIDs))
		for _, id := range diags[i].RelevantDocIDs {
			relevant[id] = true
		}

		ndcg := search.NDCGK(diags[i].Top10IDs, relevant, 10)
		diags[i].NDCG10 = &ndcg

		hit := 0
		for _, id := range diags[i].Top10IDs {
			if relevant[id] {
				hit++
			}
		}
		recall := float64(hit) / float64(len(relevant))
		diags[i].Recall10 = &recall
	}
}

func traceIDs(trace *search.SearchTrace, pick func(*search.SearchTrace) []uuid.UUID) []uuid.UUID {
	if trace == nil {
		return nil
	}
	return pick(trace)
}

// rankIndex 는 ID 목록을 1-기반 순위 맵으로 바꾼다.
func rankIndex(ids []uuid.UUID) map[string]int {
	out := make(map[string]int, len(ids))
	for i, id := range ids {
		key := id.String()
		if _, seen := out[key]; !seen {
			out[key] = i + 1
		}
	}
	return out
}

// diagnosticQueryID 는 질의 문구를 드러내지 않는 안정적인 식별자를 만든다.
// 골든셋 쌍은 golden_queries.id 를 그대로 쓰고, 피드백 기반 쌍에는 그런 id 가
// 없으므로 질의 해시의 접두사를 쓴다(실행마다 같은 값이 나온다).
func diagnosticQueryID(pair store.EvalPair) string {
	if pair.GoldenQueryID != "" {
		return pair.GoldenQueryID
	}
	return "sha256:" + digest(pair.Query)[:16]
}

// diagnosticQuerySource 는 질의 출처다. 골든셋이면 golden_queries.source
// ("seed" | "ask_history" | ...), 아니면 EvalPair.Source.
func diagnosticQuerySource(pair store.EvalPair) string {
	if pair.GoldenQuerySource != "" {
		return pair.GoldenQuerySource
	}
	return pair.Source
}

// enrichDiagnostics 는 정답 라벨 문서의 소스·보존태그·발생일을 채운다.
// 라벨 전체를 한 번의 조회로 읽어, 행마다 DB 를 때리지 않는다.
func enrichDiagnostics(diags []queryDiagnostics, facts map[string]store.EvalLabelFact) {
	for i := range diags {
		for j := range diags[i].RelevantDocs {
			fact, ok := facts[diags[i].RelevantDocs[j].DocID]
			if !ok {
				continue
			}
			diags[i].RelevantDocs[j].SourceType = fact.SourceType
			diags[i].RelevantDocs[j].Retention = fact.Retention
			if fact.OccurredAt != nil {
				diags[i].RelevantDocs[j].OccurredAt = fact.OccurredAt.Format(time.DateOnly)
			}
		}
	}
}

// writeDiagnostics 는 헤더 한 줄과 진단 행들을 JSON Lines 로 path 에 쓴다
// (dump v2, #269). 파일은 소유자만 읽고 쓸 수 있게 만들며, 이미 있던 파일이
// 더 느슨한 퍼미션이었다면 조인다.
//
// diags 는 호출 전에 attachQueryMetrics 를 거쳐야 kind/latency_ms/ndcg10/
// recall10/fp10 이 채워진다 — writeDiagnostics 자신은 그 값을 계산하지 않고
// 있는 그대로 직렬화만 한다.
func writeDiagnostics(path string, header evaldump.Header, diags []queryDiagnostics) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, dumpFileMode)
	if err != nil {
		return fmt.Errorf("eval: open dump file: %w", err)
	}
	defer file.Close()
	if err := file.Chmod(dumpFileMode); err != nil {
		return fmt.Errorf("eval: restrict dump file mode: %w", err)
	}
	enc := json.NewEncoder(file)
	header.Kind = "header"
	if header.DumpVersion == 0 {
		header.DumpVersion = evaldump.SchemaVersion
	}
	if err := enc.Encode(header); err != nil {
		return fmt.Errorf("eval: write dump header: %w", err)
	}
	for _, diag := range diags {
		if err := enc.Encode(diag); err != nil {
			return fmt.Errorf("eval: write dump line: %w", err)
		}
	}
	return file.Sync()
}
