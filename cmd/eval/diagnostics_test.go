package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/evaldump"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/baekenough/second-brain/internal/timeutil"
	"github.com/google/uuid"
)

// 평가 진단에 절대 들어가면 안 되는 문자열들. 골든 질의는 사용자가 실제로
// 물어본 문장이고 문서 제목·본문은 통화·문자 원문이다.
const (
	secretQueryText = "지난주 김철수와 통화한 내용"
	secretTitle     = "010-1234-5678 통화 녹음"
	secretContent   = "계좌번호는 110-123-456789 입니다"
)

var (
	labelDocID   = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	decoyDocID   = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	negativeDoc  = uuid.MustParse("33333333-3333-3333-3333-333333333333")
	unretrievedD = uuid.MustParse("44444444-4444-4444-4444-444444444444")
)

// tracingStub 은 SearchTraced 를 구현하는 검색기 대역이다. 마지막으로 받은
// 질의를 기록해 두어 시간창이 실제로 검색까지 전달됐는지 확인할 수 있게 한다.
type tracingStub struct {
	lastQuery chan model.SearchQuery
	fail      bool
}

func newTracingStub(fail bool) *tracingStub {
	return &tracingStub{lastQuery: make(chan model.SearchQuery, 16), fail: fail}
}

func (s *tracingStub) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	s.lastQuery <- q
	if s.fail {
		return nil, errors.New("synthetic failure")
	}
	return []*model.SearchResult{
		{Document: model.Document{ID: decoyDocID, SourceType: model.SourceSMS, Title: secretTitle, Content: secretContent}},
		{Document: model.Document{ID: labelDocID, SourceType: model.SourceCall, Title: secretTitle, Content: secretContent}},
		{Document: model.Document{ID: negativeDoc, SourceType: model.SourceGmail, Title: secretTitle, Content: secretContent}},
	}, nil
}

func (s *tracingStub) SearchTraced(ctx context.Context, q model.SearchQuery) ([]*model.SearchResult, *search.SearchTrace, error) {
	results, err := s.Search(ctx, q)
	trace := &search.SearchTrace{
		RerankRequested: q.UseRerank,
		RerankAttempted: q.UseRerank,
		// 오버페치 풀에는 회수됐지만 페이지 밖으로 밀린 문서도 들어 있다.
		PoolIDs:      []uuid.UUID{decoyDocID, labelDocID, negativeDoc},
		PreRerankIDs: []uuid.UUID{labelDocID, decoyDocID, negativeDoc},
		LaneHits: map[uuid.UUID][]string{
			labelDocID: {search.LaneDocumentStore, search.LaneChunkVector},
			decoyDocID: {search.LaneDocumentStore},
		},
	}
	return results, trace, err
}

func diagnosticTestPair() store.EvalPair {
	return store.EvalPair{
		Query:             secretQueryText,
		RelevantDocIDs:    []string{labelDocID.String(), unretrievedD.String()},
		IrrelevantDocIDs:  []string{negativeDoc.String()},
		Source:            "golden",
		GoldenQueryID:     "aaaaaaaa-0000-0000-0000-000000000001",
		GoldenQuerySource: "seed",
	}
}

// 진단 파일은 사람 손을 타고 돌아다닌다. 질의 문구·문서 제목·본문이 한 글자라도
// 섞이면 파일 전체가 개인정보가 되므로, 직렬화된 바이트에서 직접 확인한다.
func TestDumpFileCarriesNoQueryOrDocumentText(t *testing.T) {
	evaluated := evaluatePairs(context.Background(), newTracingStub(false),
		[]store.EvalPair{diagnosticTestPair()}, evalRunOptions{rerank: true, diagnose: true})
	if len(evaluated.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %d rows, want 1", len(evaluated.Diagnostics))
	}
	occurred := time.Date(2026, 8, 3, 14, 30, 0, 0, timeutil.KST())
	enrichDiagnostics(evaluated.Diagnostics, map[string]store.EvalLabelFact{
		labelDocID.String(): {Found: true, Status: "active", SourceType: "call", Retention: "keep", OccurredAt: &occurred},
	})

	attachQueryMetrics(evaluated.Diagnostics, evaluated.Latencies)
	path := filepath.Join(t.TempDir(), "dump.jsonl")
	header := evaldump.Header{
		DumpVersion: evaldump.SchemaVersion, LabelHash: "lh", ConfigHash: "ch", CodeRevision: "rev",
		Attempted: 1, Failed: 0, LabelSource: "golden-user", Split: "all", WindowMode: "none",
	}
	if err := writeDiagnostics(path, header, evaluated.Diagnostics); err != nil {
		t.Fatalf("writeDiagnostics: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	for _, secret := range []string{secretQueryText, secretTitle, secretContent, "110-123-456789", "010-1234-5678"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("dump leaked %q", secret)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat dump: %v", err)
	}
	if info.Mode().Perm() != dumpFileMode {
		t.Fatalf("dump mode = %v, want %v", info.Mode().Perm(), dumpFileMode)
	}

	// 파일은 JSON Lines 다 — 1 번째 줄은 헤더, 그다음 한 줄에 한 질의.
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	var lines [][]byte
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		lines = append(lines, line)
	}
	if len(lines) != 2 {
		t.Fatalf("dump has %d lines, want 2 (header + 1 query row)", len(lines))
	}
	var gotHeader evaldump.Header
	if err := json.Unmarshal(lines[0], &gotHeader); err != nil {
		t.Fatalf("line 1 is not a valid header: %v", err)
	}
	if gotHeader.Kind != "header" || gotHeader.DumpVersion != evaldump.SchemaVersion {
		t.Fatalf("header shape wrong: %+v", gotHeader)
	}
	if gotHeader.LabelHash != "lh" || gotHeader.ConfigHash != "ch" || gotHeader.CodeRevision != "rev" {
		t.Fatalf("header provenance lost: %+v", gotHeader)
	}

	var diag queryDiagnostics
	if err := json.Unmarshal(lines[1], &diag); err != nil {
		t.Fatalf("line 2 is not a valid query row: %v", err)
	}
	if diag.Kind != "query" {
		t.Fatalf("row kind = %q, want %q", diag.Kind, "query")
	}
	if diag.LatencyMs == nil {
		t.Fatal("latency_ms was not attached")
	}
	if diag.NDCG10 == nil || diag.Recall10 == nil {
		t.Fatalf("ndcg10/recall10 must be set for a query with positive labels: %+v", diag)
	}
	if diag.FP10 == nil || *diag.FP10 != diag.NegativeInTop10 {
		t.Fatalf("fp10 = %v, want mirror of negative_ids_in_top10 = %d", diag.FP10, diag.NegativeInTop10)
	}

	if diag.QueryID != "aaaaaaaa-0000-0000-0000-000000000001" || diag.QuerySource != "seed" {
		t.Fatalf("query provenance lost: %+v", diag)
	}
	if diag.QueryLen != len([]rune(secretQueryText)) {
		t.Fatalf("query_len = %d, want %d", diag.QueryLen, len([]rune(secretQueryText)))
	}
	if diag.NegativeInTop10 != 1 {
		t.Fatalf("negative_ids_in_top10 = %d, want 1", diag.NegativeInTop10)
	}
	if len(diag.Top10IDs) != 3 || diag.Top10SourceTypes[1] != string(model.SourceCall) {
		t.Fatalf("top10 shape wrong: %+v", diag)
	}
	if diag.OverfetchPoolSize != 3 || !diag.RerankRequested || !diag.RerankAttempted || diag.RerankFailed {
		t.Fatalf("pool/rerank fields wrong: %+v", diag)
	}

	// 회수된 정답과 회수조차 안 된 정답이 서로 다르게 기록돼야 한다 —
	// 이 구분이 없으면 NDCG 0 을 보고도 원인을 못 가린다.
	byID := map[string]dumpLabel{}
	for _, label := range diag.RelevantDocs {
		byID[label.DocID] = label
	}
	hit := byID[labelDocID.String()]
	if hit.FinalRank == nil || *hit.FinalRank != 2 {
		t.Fatalf("final_rank = %v, want 2", hit.FinalRank)
	}
	if hit.PreRerankRank == nil || *hit.PreRerankRank != 1 {
		t.Fatalf("pre_rerank_rank = %v, want 1", hit.PreRerankRank)
	}
	if !hit.InOverfetchPool || len(hit.LanesHit) != 2 {
		t.Fatalf("pool/lane evidence lost: %+v", hit)
	}
	if hit.SourceType != "call" || hit.Retention != "keep" || hit.OccurredAt != "2026-08-03" {
		t.Fatalf("label facts wrong: %+v", hit)
	}

	miss := byID[unretrievedD.String()]
	if miss.FinalRank != nil || miss.PreRerankRank != nil || miss.InOverfetchPool || len(miss.LanesHit) != 0 {
		t.Fatalf("unretrieved label must stay empty, got %+v", miss)
	}
	// 시각은 날짜까지만 — 초 단위가 남으면 특정 통화를 지목하는 단서가 된다.
	if strings.Contains(string(raw), "14:30") {
		t.Fatal("dump leaked a time-of-day")
	}
}

// 검색이 실패한 질의도 진단 행을 남겨야 한다. 점수가 0 으로 나온 실행이
// 바로 무엇이 어디까지 올라왔는지 가장 궁금해지는 순간이다.
func TestDumpKeepsRowsForFailedSearches(t *testing.T) {
	evaluated := evaluatePairs(context.Background(), newTracingStub(true),
		[]store.EvalPair{diagnosticTestPair()}, evalRunOptions{rerank: false, diagnose: true})
	if len(evaluated.Diagnostics) != 1 || !evaluated.Diagnostics[0].SearchFailed {
		t.Fatalf("failed query lost its diagnostics row: %+v", evaluated.Diagnostics)
	}
	if len(evaluated.Diagnostics[0].Top10IDs) != 0 {
		t.Fatalf("failed search reported results: %+v", evaluated.Diagnostics[0])
	}
}

// --dump 를 켜지 않으면 진단은 아예 모이지 않는다(기존 동작 보존).
func TestDiagnosticsAreNotCollectedByDefault(t *testing.T) {
	evaluated := evaluatePairs(context.Background(), newTracingStub(false),
		[]store.EvalPair{diagnosticTestPair()}, evalRunOptions{rerank: true})
	if evaluated.Diagnostics != nil {
		t.Fatalf("diagnostics collected without --dump: %+v", evaluated.Diagnostics)
	}
}

// 골든셋 id 가 없는 피드백 기반 쌍도 질의 문구를 드러내지 않고 지목할 수
// 있어야 하며, 그 식별자는 실행마다 같아야 한다.
func TestFeedbackPairGetsStablePseudonymousQueryID(t *testing.T) {
	pair := store.EvalPair{Query: secretQueryText, Source: "feedback"}
	id := diagnosticQueryID(pair)
	if id != diagnosticQueryID(pair) {
		t.Fatal("query id is not stable across calls")
	}
	if strings.Contains(id, secretQueryText) || !strings.HasPrefix(id, "sha256:") {
		t.Fatalf("query id leaks or is malformed: %q", id)
	}
	if got := diagnosticQuerySource(pair); got != "feedback" {
		t.Fatalf("query_source = %q, want feedback", got)
	}
}

// 정답 라벨이 없는 질의는 ndcg10/recall10 이 0 이 아니라 null 로 남아야 한다 —
// "잴 대상 없음"과 "쟀더니 0 점"은 다른 사실이다. fp10 은 라벨과 무관하게
// 항상 채워진다.
func TestAttachQueryMetricsLeavesNDCGNullWithoutPositives(t *testing.T) {
	diags := []queryDiagnostics{
		{Top10IDs: []string{"a", "b"}, RelevantDocIDs: nil, NegativeInTop10: 2},
	}
	attachQueryMetrics(diags, []float64{7.5})
	if diags[0].Kind != "query" {
		t.Fatalf("kind = %q, want query", diags[0].Kind)
	}
	if diags[0].LatencyMs == nil || *diags[0].LatencyMs != 7.5 {
		t.Fatalf("latency_ms = %v, want 7.5", diags[0].LatencyMs)
	}
	if diags[0].NDCG10 != nil || diags[0].Recall10 != nil {
		t.Fatalf("ndcg10/recall10 must stay nil without positives, got ndcg10=%v recall10=%v",
			diags[0].NDCG10, diags[0].Recall10)
	}
	if diags[0].FP10 == nil || *diags[0].FP10 != 2 {
		t.Fatalf("fp10 = %v, want 2", diags[0].FP10)
	}
}

// NDCG10 은 search.NDCGK 를 직접 호출해 만든다 — Aggregate 가 macro-average 를
// 낼 때 쓰는 것과 같은 함수다. 이 테스트는 그 값이 손으로 계산한 값과
// 일치하는지, 그리고 Recall10 이 |rel ∩ top10| / |rel| 인지 확인한다.
func TestAttachQueryMetricsComputesNDCGAndRecall(t *testing.T) {
	diags := []queryDiagnostics{
		// top10 = [x, rel1, y, rel2]; rel = {rel1, rel2, rel3(미회수)}.
		{Top10IDs: []string{"x", "rel1", "y", "rel2"}, RelevantDocIDs: []string{"rel1", "rel2", "rel3"}},
	}
	attachQueryMetrics(diags, []float64{1})

	wantDCG := 1/math.Log2(3) + 1/math.Log2(5) // rel1 at rank 2, rel2 at rank 4
	wantIDCG := 1/math.Log2(2) + 1/math.Log2(3) + 1/math.Log2(4)
	wantNDCG := wantDCG / wantIDCG
	if diags[0].NDCG10 == nil || math.Abs(*diags[0].NDCG10-wantNDCG) > 1e-9 {
		t.Fatalf("ndcg10 = %v, want %v", diags[0].NDCG10, wantNDCG)
	}
	wantRecall := 2.0 / 3.0
	if diags[0].Recall10 == nil || math.Abs(*diags[0].Recall10-wantRecall) > 1e-9 {
		t.Fatalf("recall10 = %v, want %v", diags[0].Recall10, wantRecall)
	}
}

// writeDiagnostics 가 쓴 v2 덤프는 internal/evaldump.Read 로 그대로 읽혀야
// 한다 — 완료 기준 6번(round trip).
func TestDumpV2RoundTripsThroughEvaldumpReader(t *testing.T) {
	evaluated := evaluatePairs(context.Background(), newTracingStub(false),
		[]store.EvalPair{diagnosticTestPair()}, evalRunOptions{rerank: true, diagnose: true})
	attachQueryMetrics(evaluated.Diagnostics, evaluated.Latencies)

	path := filepath.Join(t.TempDir(), "roundtrip.jsonl")
	header := evaldump.Header{
		DumpVersion: evaldump.SchemaVersion, LabelHash: "lh-rt", ConfigHash: "ch-rt", CodeRevision: "rev-rt",
		Attempted: 1, Failed: 0, LabelSource: "golden-user", Split: "all", WindowMode: "plan",
		CreatedAt: "2026-09-24T00:00:00Z",
	}
	if err := writeDiagnostics(path, header, evaluated.Diagnostics); err != nil {
		t.Fatalf("writeDiagnostics: %v", err)
	}

	dump, err := evaldump.Read(path)
	if err != nil {
		t.Fatalf("evaldump.Read: %v", err)
	}
	if dump.Version != evaldump.SchemaVersion {
		t.Fatalf("Version = %d, want %d", dump.Version, evaldump.SchemaVersion)
	}
	if dump.Header == nil || dump.Header.LabelHash != "lh-rt" || dump.Header.ConfigHash != "ch-rt" ||
		dump.Header.CodeRevision != "rev-rt" || dump.Header.WindowMode != "plan" {
		t.Fatalf("header did not round trip: %+v", dump.Header)
	}
	if len(dump.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(dump.Rows))
	}
	row := dump.Rows[0]
	if row.QueryID != "aaaaaaaa-0000-0000-0000-000000000001" || row.Kind != "query" {
		t.Fatalf("row identity did not round trip: %+v", row)
	}
	if row.LatencyMs == nil || row.NDCG10 == nil || row.Recall10 == nil || row.FP10 == nil {
		t.Fatalf("v2 metrics did not round trip: %+v", row)
	}
	if len(row.RelevantDocs) != 2 {
		t.Fatalf("relevant_docs did not round trip: %+v", row.RelevantDocs)
	}
}
