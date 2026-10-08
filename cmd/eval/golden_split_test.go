package main

import (
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/store"
)

// 상대 기간 질문의 날짜 분리(golden_split.go). 모든 데이터는 가상이다.

const (
	splitTemporalQ = "내일 일정 뭐야"
	splitPlainQ    = "zz 가상 거래처 연락처"
	splitQID       = "00000000-0000-4000-8000-000000000001"
	splitQID2      = "00000000-0000-4000-8000-000000000002"
)

func jrow(text, qid string, asked, judged time.Time, doc, judgment string) store.GoldenJudgmentRow {
	return store.GoldenJudgmentRow{QueryText: text, QueryID: qid, QuerySource: "ask_history",
		AskedAt: asked, DocumentID: doc, Judgment: judgment, JudgedAt: judged}
}

// 두 판정일: 2026-08-17 23:00 KST(=14:00Z), 2026-08-18 00:30 KST(=15:30Z) — 자정
// 직후라 UTC 로는 같은 날이지만 KST 로는 다른 날이다.
var (
	splitAsked = time.Date(2026, 8, 17, 1, 0, 0, 0, time.UTC)
	splitDay1  = time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	splitDay2  = time.Date(2026, 8, 17, 15, 30, 0, 0, time.UTC)
)

func TestGoldenSplit_FixturesAreWhatTheyClaim(t *testing.T) {
	t.Parallel()
	if !hasPeriodExpression(splitTemporalQ, splitDay1) {
		t.Fatal("fixture: 상대 기간 질문을 파서가 기간으로 보지 않는다")
	}
	if hasPeriodExpression(splitPlainQ, splitDay1) {
		t.Fatal("fixture: 기간 없는 질문을 파서가 기간으로 본다")
	}
}

func TestGoldenSplit_TemporalMultiDaySplits(t *testing.T) {
	t.Parallel()
	rows := []store.GoldenJudgmentRow{
		jrow(splitTemporalQ, splitQID, splitAsked, splitDay1, "doc-a", "relevant"),
		jrow(splitTemporalQ, splitQID, splitAsked, splitDay1.Add(time.Minute), "doc-n", "noise"),
		jrow(splitTemporalQ, splitQID, splitAsked, splitDay2, "doc-b", "relevant"),
	}
	got := goldenPairsFromJudgments(rows, windowAnchorJudgedAt)
	if len(got) != 2 {
		t.Fatalf("쌍 %d개, want 2 (KST 날짜 둘)", len(got))
	}
	// CreatedAt 내림차순 → 둘째 날이 먼저.
	want := []struct {
		id      int64
		qid     string
		created time.Time
		pos     []string
		neg     []string
	}{
		{1, splitQID + "@2026-08-18", splitDay2, []string{"doc-b"}, []string{}},
		{2, splitQID + "@2026-08-17", splitDay1, []string{"doc-a"}, []string{"doc-n"}},
	}
	for i, w := range want {
		p := got[i]
		if p.ID != w.id || p.GoldenQueryID != w.qid || !p.CreatedAt.Equal(w.created) ||
			!reflect.DeepEqual(p.RelevantDocIDs, w.pos) || !reflect.DeepEqual(p.IrrelevantDocIDs, w.neg) {
			t.Errorf("pair[%d] = id=%d qid=%s created=%v pos=%v neg=%v", i, p.ID, p.GoldenQueryID, p.CreatedAt, p.RelevantDocIDs, p.IrrelevantDocIDs)
		}
		if p.Query != splitTemporalQ || p.Source != "golden" {
			t.Errorf("pair[%d]: query/source 가 바뀌었다", i)
		}
	}
	// 각 쌍의 기준 시각이 자기 날짜에 있어야 창이 그 날 기준으로 풀린다.
	anchor := windowAnchorFor(windowAnchorJudgedAt, time.Time{})
	if !anchor(got[0]).Equal(splitDay2) || !anchor(got[1]).Equal(splitDay1) {
		t.Error("분리된 쌍의 기준 시각이 자기 날짜가 아니다")
	}
}

func TestGoldenSplit_NoSplitCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rows []store.GoldenJudgmentRow
		kind string
	}{
		{"상대 기간 질문이지만 하루", []store.GoldenJudgmentRow{
			jrow(splitTemporalQ, splitQID, splitAsked, splitDay1, "doc-a", "relevant"),
			jrow(splitTemporalQ, splitQID, splitAsked, splitDay1.Add(time.Hour/2), "doc-b", "relevant"),
		}, windowAnchorJudgedAt},
		{"기간 없는 질문은 여러 날이어도 묶는다", []store.GoldenJudgmentRow{
			jrow(splitPlainQ, splitQID, splitAsked, splitDay1, "doc-a", "relevant"),
			jrow(splitPlainQ, splitQID, splitAsked, splitDay2, "doc-b", "relevant"),
		}, windowAnchorJudgedAt},
		{"asked_at 기준: 같은 질의 행이면 판정일이 달라도 하나", []store.GoldenJudgmentRow{
			jrow(splitTemporalQ, splitQID, splitAsked, splitDay1, "doc-a", "relevant"),
			jrow(splitTemporalQ, splitQID, splitAsked, splitDay2, "doc-b", "relevant"),
		}, windowAnchorAskedAt},
	}
	for _, tc := range tests {
		got := goldenPairsFromJudgments(tc.rows, tc.kind)
		if len(got) != 1 {
			t.Fatalf("%s: 쌍 %d개, want 1", tc.name, len(got))
		}
		p := got[0]
		if p.ID != 1 || p.GoldenQueryID != splitQID || !reflect.DeepEqual(p.RelevantDocIDs, []string{"doc-a", "doc-b"}) ||
			!p.CreatedAt.Equal(tc.rows[0].JudgedAt) {
			t.Errorf("%s: %+v", tc.name, p)
		}
	}
}

// asked_at 기준에서는 같은 문구의 서로 다른 질의 행이 다른 날 물어졌을 때 나뉜다.
func TestGoldenSplit_AskedAtSplitsByQueryRow(t *testing.T) {
	t.Parallel()
	asked2 := splitAsked.Add(48 * time.Hour)
	rows := []store.GoldenJudgmentRow{
		jrow(splitTemporalQ, splitQID, splitAsked, splitDay1, "doc-a", "relevant"),
		jrow(splitTemporalQ, splitQID2, asked2, splitDay1.Add(time.Minute), "doc-b", "relevant"),
	}
	if n := len(goldenPairsFromJudgments(rows, windowAnchorJudgedAt)); n != 1 {
		t.Fatalf("judged_at 기준(같은 날 판정)인데 %d개로 나뉘었다", n)
	}
	got := goldenPairsFromJudgments(rows, windowAnchorAskedAt)
	if len(got) != 2 {
		t.Fatalf("asked_at 기준 쌍 %d개, want 2", len(got))
	}
	for _, p := range got {
		switch p.GoldenQueryID {
		case splitQID + "@2026-08-17":
			if !p.AskedAt.Equal(splitAsked) || p.RelevantDocIDs[0] != "doc-a" {
				t.Errorf("첫 질의 행 쌍: %+v", p)
			}
		case splitQID2 + "@2026-08-19":
			if !p.AskedAt.Equal(asked2) || p.RelevantDocIDs[0] != "doc-b" {
				t.Errorf("둘째 질의 행 쌍: %+v", p)
			}
		default:
			t.Errorf("예상 밖 query id %q", p.GoldenQueryID)
		}
	}
}

// 입력 순서와 무관하게 같은 쌍·ID·label_hash 가 나와야 한다.
func TestGoldenSplit_DeterministicIDsAndLabelHash(t *testing.T) {
	t.Parallel()
	rows := []store.GoldenJudgmentRow{
		jrow(splitTemporalQ, splitQID, splitAsked, splitDay1, "doc-a", "relevant"),
		jrow(splitTemporalQ, splitQID, splitAsked, splitDay2, "doc-b", "relevant"),
		jrow(splitTemporalQ, splitQID, splitAsked, splitDay2.Add(time.Minute), "doc-c", "irrelevant"),
		jrow(splitPlainQ, splitQID2, splitAsked, splitDay1, "doc-d", "relevant"),
		jrow(splitPlainQ, splitQID2, splitAsked, splitDay2, "doc-e", "relevant"),
	}
	base := goldenPairsFromJudgments(rows, windowAnchorJudgedAt)
	baseHash := labelFingerprint(base)
	baseSubset := deterministicSubset(base, 0)
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 20; i++ {
		shuffled := append([]store.GoldenJudgmentRow(nil), rows...)
		r.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		got := goldenPairsFromJudgments(shuffled, windowAnchorJudgedAt)
		if !reflect.DeepEqual(got, base) {
			t.Fatalf("입력 순서에 따라 쌍이 달라졌다")
		}
		// labelFingerprint 는 같은 문구의 쌍이 어떤 순서로 와도 같아야 한다.
		rev := append([]store.EvalPair(nil), got...)
		for a, b := 0, len(rev)-1; a < b; a, b = a+1, b-1 {
			rev[a], rev[b] = rev[b], rev[a]
		}
		if labelFingerprint(rev) != baseHash {
			t.Fatal("label_hash 가 쌍 순서에 따라 달라졌다")
		}
		if !reflect.DeepEqual(deterministicSubset(rev, 0), baseSubset) {
			t.Fatal("deterministicSubset 이 같은 문구 쌍의 순서를 고정하지 않는다")
		}
	}
	if len(base) != 3 {
		t.Fatalf("쌍 %d개, want 3 (상대 기간 질문 2 + 기간 없는 질문 1)", len(base))
	}
}

// 분리가 없는 입력에서 label_hash 는 예전 경로(문구 하나 = 쌍 하나)와 같아야 한다.
func TestGoldenSplit_NoSplitKeepsLabelHash(t *testing.T) {
	t.Parallel()
	rows := []store.GoldenJudgmentRow{
		jrow(splitPlainQ, splitQID, splitAsked, splitDay1, "doc-b", "relevant"),
		jrow(splitPlainQ, splitQID, splitAsked, splitDay2, "doc-a", "relevant"),
		jrow(splitTemporalQ, splitQID2, splitAsked, splitDay1, "doc-c", "noise"),
	}
	got := goldenPairsFromJudgments(rows, windowAnchorJudgedAt)
	legacy := []store.EvalPair{
		{Query: splitPlainQ, RelevantDocIDs: []string{"doc-a", "doc-b"}, IrrelevantDocIDs: []string{}},
		{Query: splitTemporalQ, RelevantDocIDs: []string{}, IrrelevantDocIDs: []string{"doc-c"}},
	}
	if labelFingerprint(got) != labelFingerprint(legacy) {
		t.Error("분리가 없는데 label_hash 가 달라졌다")
	}
}

func TestGoldenSplitApplies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode, anchor string
		want         bool
	}{
		{windowModePlan, windowAnchorJudgedAt, true},
		{windowModePlan, windowAnchorAskedAt, true},
		{windowModePlan, windowAnchorAsOf, false},
		{windowModeNone, windowAnchorJudgedAt, false},
		{windowModeNone, "", false},
	} {
		if got := goldenSplitApplies(tc.mode, tc.anchor); got != tc.want {
			t.Errorf("goldenSplitApplies(%q, %q) = %v, want %v", tc.mode, tc.anchor, got, tc.want)
		}
	}
}
