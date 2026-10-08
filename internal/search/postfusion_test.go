package search

import (
	"context"
	"math"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/timeutil"
	"github.com/google/uuid"
)

// --- 공통 fixture ---

func pfID(n int) uuid.UUID {
	var u uuid.UUID
	u[14], u[15] = byte(n>>8), byte(n)
	return u
}

// pfAt 은 2026-09-<day> <hour>:00 KST 다.
func pfAt(day, hour int) *time.Time {
	t := time.Date(2026, 9, day, hour, 0, 0, 0, timeutil.KST())
	return &t
}

func pfDoc(n int, st model.SourceType, score float64, at *time.Time) *model.SearchResult {
	return &model.SearchResult{
		Document: model.Document{ID: pfID(n), SourceType: st, OccurredAt: at, Metadata: map[string]any{}},
		Score:    score,
	}
}

// pfSMS 는 SourceID 에 상대 해시를 담은 문자 문서다(smsmap 형식).
func pfSMS(n int, contactHash string, score float64, at *time.Time) *model.SearchResult {
	r := pfDoc(n, model.SourceSMS, score, at)
	r.SourceID = "sms:1700000000000:" + contactHash + ":incoming"
	return r
}

func pfIDs(results []*model.SearchResult) []uuid.UUID { return resultIDs(results) }

func pfSortedIDs(results []*model.SearchResult) []uuid.UUID { return pfSortIDs(resultIDs(results)) }

func pfSortIDs(ids []uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func pfShuffled(results []*model.SearchResult, seed int64) []*model.SearchResult {
	out := slices.Clone(results)
	rand.New(rand.NewSource(seed)).Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// sortedCopy 는 파이프라인이 변환에 넘기는 형태(sortByScore 총순서)를 만든다.
func sortedCopy(results []*model.SearchResult) []*model.SearchResult {
	out := slices.Clone(results)
	sortByScore(out)
	return out
}

func assertSameIDs(t *testing.T, got, want []uuid.UUID) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("order mismatch\n got: %v\nwant: %v", got, want)
	}
}

// --- 접기 / 펼치기 ---

func TestCollapseContactDay_GroupsAndExpands(t *testing.T) {
	t.Parallel()
	a1 := pfSMS(1, "aaaa", 10, pfAt(1, 9))
	b := pfSMS(2, "bbbb", 9, pfAt(1, 10))
	a2 := pfSMS(3, "aaaa", 8, pfAt(1, 11))
	c := pfDoc(4, model.SourceGmail, 7, pfAt(1, 12))
	a3 := pfSMS(5, "aaaa", 5, pfAt(1, 20))
	in := []*model.SearchResult{a1, b, a2, c, a3}

	ranked, set := collapseContactDay(in, 3)
	assertSameIDs(t, pfIDs(ranked), []uuid.UUID{pfID(1), pfID(2), pfID(4)})
	assertSameIDs(t, pfIDs(expandCollapsed(ranked, set)), []uuid.UUID{pfID(1), pfID(3), pfID(5), pfID(2), pfID(4)})

	// 펼침 상한 1: 초과분(a3)은 버리지 않고 맨 뒤로.
	ranked, set = collapseContactDay(in, 1)
	assertSameIDs(t, pfIDs(expandCollapsed(ranked, set)), []uuid.UUID{pfID(1), pfID(3), pfID(2), pfID(4), pfID(5)})

	// 리랭커가 대표 순서를 뒤집어도 멤버는 자기 대표를 따라간다.
	reversed := []*model.SearchResult{ranked[2], ranked[1], ranked[0]}
	assertSameIDs(t, pfIDs(expandCollapsed(reversed, set)), []uuid.UUID{pfID(4), pfID(2), pfID(1), pfID(3), pfID(5)})
}

func TestCollapseContactDay_RepresentativeIsBestScored(t *testing.T) {
	t.Parallel()
	// 입력 순서가 점수 순이 아니어도 대표는 점수 최상위 문서이고, 대표는
	// 자기 입력 위치를 지킨다.
	low := pfSMS(1, "aaaa", 1, pfAt(2, 9))
	other := pfDoc(2, model.SourceNote, 5, nil)
	high := pfSMS(3, "aaaa", 9, pfAt(2, 10))
	ranked, set := collapseContactDay([]*model.SearchResult{low, other, high}, 3)
	assertSameIDs(t, pfIDs(ranked), []uuid.UUID{pfID(2), pfID(3)})
	assertSameIDs(t, pfIDs(set.groups[0].members), []uuid.UUID{pfID(1)})
}

func TestCollapseContactDay_KeyRules(t *testing.T) {
	t.Parallel()
	// KST 날짜 경계: UTC 로는 같은 날(09-01 14:30Z / 15:30Z)이지만 KST 로는 다른 날.
	utcA := time.Date(2026, 9, 1, 14, 30, 0, 0, time.UTC) // KST 09-01 23:30
	utcB := time.Date(2026, 9, 1, 15, 30, 0, 0, time.UTC) // KST 09-02 00:30
	night := pfSMS(1, "aaaa", 5, &utcA)
	morning := pfSMS(2, "aaaa", 4, &utcB)

	// metadata.number_hash 가 SourceID 해시보다 우선한다.
	h1 := pfDoc(3, model.SourceCall, 3, pfAt(3, 9))
	h1.Metadata["number_hash"] = "zzzz"
	h1.SourceID = "call-log:1:xxxx:d"
	h2 := pfDoc(4, model.SourceCall, 2, pfAt(3, 18))
	h2.Metadata["number_hash"] = "zzzz"
	h2.SourceID = "call-log:2:yyyy:d"

	// thread_id 는 숫자(float64)로 와도 키가 된다.
	t1 := pfDoc(5, model.SourceSMS, 1.5, pfAt(4, 9))
	t1.Metadata["thread_id"] = float64(42)
	t2 := pfDoc(6, model.SourceSMS, 1.4, pfAt(4, 10))
	t2.Metadata["thread_id"] = float64(42)

	// 같은 해시라도 문자와 통화는 다른 그룹.
	smsX := pfSMS(7, "qqqq", 1.3, pfAt(5, 9))
	callX := pfDoc(8, model.SourceCall, 1.2, pfAt(5, 10))
	callX.SourceID = "call-log:1:qqqq:d"

	// 키 없는 문서: 메타데이터 없음, occurred_at 없음, 다른 소스.
	noMeta := pfDoc(9, model.SourceSMS, 1.1, pfAt(6, 9))
	noMeta.Metadata = nil
	noMeta2 := pfDoc(10, model.SourceSMS, 1.0, pfAt(6, 10))
	noTime := pfSMS(11, "aaaa", 0.9, nil)
	noTime2 := pfSMS(12, "aaaa", 0.8, nil)
	mail1 := pfDoc(13, model.SourceGmail, 0.7, pfAt(7, 9))
	mail1.Metadata["thread_id"] = "t"
	mail2 := pfDoc(14, model.SourceGmail, 0.6, pfAt(7, 10))
	mail2.Metadata["thread_id"] = "t"

	in := []*model.SearchResult{night, morning, h1, h2, t1, t2, smsX, callX, noMeta, noMeta2, noTime, noTime2, mail1, mail2}
	ranked, set := collapseContactDay(in, 3)
	// 접히는 것은 h2(number_hash 그룹)와 t2(thread_id 그룹)뿐이다.
	want := []uuid.UUID{pfID(1), pfID(2), pfID(3), pfID(5), pfID(7), pfID(8), pfID(9), pfID(10), pfID(11), pfID(12), pfID(13), pfID(14)}
	assertSameIDs(t, pfIDs(ranked), want)
	if len(set.groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(set.groups))
	}
	assertSameIDs(t, pfIDs(expandCollapsed(ranked, set)), pfIDs(in))
}

func TestCollapseContactDay_EdgeCases(t *testing.T) {
	t.Parallel()
	for name, in := range map[string][]*model.SearchResult{
		"empty":  nil,
		"single": {pfSMS(1, "aaaa", 1, pfAt(1, 1))},
		"no groups": {
			pfSMS(1, "aaaa", 2, pfAt(1, 1)),
			pfSMS(2, "bbbb", 1, pfAt(1, 1)),
		},
	} {
		ranked, set := collapseContactDay(in, 3)
		if set != nil {
			t.Errorf("%s: expected no collapse set", name)
		}
		assertSameIDs(t, pfIDs(ranked), pfIDs(in))
		assertSameIDs(t, pfIDs(expandCollapsed(ranked, nil)), pfIDs(in))
	}
	// 노브 꺼짐·recent 정렬은 손대지 않는다.
	in := []*model.SearchResult{pfSMS(1, "aaaa", 2, pfAt(1, 1)), pfSMS(2, "aaaa", 1, pfAt(1, 2))}
	if _, set := collapseForRanking(model.SearchQuery{}, in, model.SearchTuning{}); set != nil {
		t.Error("collapse ran with the knob off")
	}
	on := model.SearchTuning{CollapseContactDay: true}.Normalized()
	if _, set := collapseForRanking(model.SearchQuery{Sort: model.SortRecent}, in, on); set != nil {
		t.Error("collapse ran on a Sort=recent query")
	}
	if _, set := collapseForRanking(model.SearchQuery{}, in, on); set == nil {
		t.Error("collapse did not run with the knob on (positive control)")
	}
}

// pfRandomPool 은 그룹·키 없는 문서·NaN 점수·nil occurred_at 이 섞인 후보다.
func pfRandomPool(seed int64, n int) []*model.SearchResult {
	rng := rand.New(rand.NewSource(seed))
	out := make([]*model.SearchResult, n)
	hashes := []string{"aaaa", "bbbb", "cccc"}
	for i := range out {
		score := float64(rng.Intn(6)) // 동점이 흔하다
		if rng.Intn(10) == 0 {
			score = math.NaN()
		}
		var at *time.Time
		if rng.Intn(8) != 0 {
			at = pfAt(1+rng.Intn(3), rng.Intn(24))
		}
		switch rng.Intn(4) {
		case 0:
			out[i] = pfDoc(i+1, model.SourceGmail, score, at)
		case 1:
			r := pfDoc(i+1, model.SourceCall, score, at)
			r.SourceID = "call-log:1:" + hashes[rng.Intn(len(hashes))] + ":d"
			out[i] = r
		default:
			out[i] = pfSMS(i+1, hashes[rng.Intn(len(hashes))], score, at)
		}
		if rng.Intn(5) == 0 {
			out[i].Embedding = []float32{rng.Float32(), rng.Float32(), rng.Float32()}
		}
	}
	return out
}

func TestCollapseExpand_NeverLosesCandidatesAndIsDeterministic(t *testing.T) {
	t.Parallel()
	for seed := int64(1); seed <= 50; seed++ {
		pool := pfRandomPool(seed, 40)
		sorted := sortedCopy(pool)
		for _, expandMax := range []int{0, 1, 3, 20} {
			ranked, set := collapseContactDay(sorted, expandMax)
			if len(ranked) > len(sorted) {
				t.Fatalf("seed %d: ranked grew", seed)
			}
			// 리랭커 역할: 대표 순서를 임의로 섞는다.
			reranked := pfShuffled(ranked, seed)
			out := expandCollapsed(reranked, set)
			if len(out) != len(pool) {
				t.Fatalf("seed %d max %d: size %d, want %d", seed, expandMax, len(out), len(pool))
			}
			assertSameIDs(t, pfSortedIDs(out), pfSortedIDs(pool))

			// 같은 후보를 다른 순서로 받아도(파이프라인은 sortByScore 후 넘긴다)
			// 결과가 같아야 한다.
			ranked2, set2 := collapseContactDay(sortedCopy(pfShuffled(pool, seed+1000)), expandMax)
			assertSameIDs(t, pfIDs(ranked2), pfIDs(ranked))
			assertSameIDs(t, pfIDs(expandCollapsed(pfShuffled(ranked2, seed), set2)), pfIDs(out))
		}
	}
}

// --- 날짜 버킷 다변화 ---

func TestWindowSpansMultipleDays(t *testing.T) {
	t.Parallel()
	day1, day2 := pfAt(1, 0), pfAt(2, 0)
	day3 := pfAt(3, 0)
	tests := []struct {
		name     string
		from, to *time.Time
		want     bool
	}{
		{"no window", nil, nil, false},
		{"lower bound only", day1, nil, true},
		{"upper bound only", nil, day1, true},
		{"exactly one KST day [d1, d2)", day1, day2, false},
		{"two KST days", day1, day3, true},
		{"empty window", day2, day1, false},
		{"zero-length window", day1, day1, false},
	}
	for _, tc := range tests {
		got := windowSpansMultipleDays(model.SearchQuery{OccurredFrom: tc.from, OccurredTo: tc.to})
		if got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDiversifyByDay(t *testing.T) {
	t.Parallel()
	in := []*model.SearchResult{
		pfDoc(1, model.SourceCall, 10, pfAt(1, 9)),
		pfDoc(2, model.SourceCall, 9, pfAt(1, 10)),
		pfDoc(3, model.SourceSMS, 8, pfAt(2, 9)),
		pfDoc(4, model.SourceCall, 7, pfAt(1, 11)),
		pfDoc(5, model.SourceGmail, 6, nil), // 날짜 없음: 첫 바퀴에서 빠진다
		pfDoc(6, model.SourceCalendar, 5, pfAt(3, 9)),
		pfDoc(7, model.SourceCall, math.NaN(), pfAt(4, 9)),
	}
	got := diversifyByDay(in)
	assertSameIDs(t, pfIDs(got), []uuid.UUID{pfID(1), pfID(3), pfID(6), pfID(7), pfID(2), pfID(4), pfID(5)})
	assertSameIDs(t, pfIDs(diversifyByDay(nil)), nil)
	assertSameIDs(t, pfIDs(diversifyByDay(in[:1])), []uuid.UUID{pfID(1)})

	for seed := int64(1); seed <= 30; seed++ {
		pool := pfRandomPool(seed, 30)
		a := diversifyByDay(sortedCopy(pool))
		b := diversifyByDay(sortedCopy(pfShuffled(pool, seed)))
		assertSameIDs(t, pfIDs(a), pfIDs(b))
		assertSameIDs(t, pfSortedIDs(a), pfSortedIDs(pool))
	}
}

func TestApplyPostRerank_BucketNeedsMultiDayWindow(t *testing.T) {
	t.Parallel()
	in := []*model.SearchResult{
		pfDoc(1, model.SourceCall, 3, pfAt(1, 9)),
		pfDoc(2, model.SourceCall, 2, pfAt(1, 10)),
		pfDoc(3, model.SourceCall, 1, pfAt(2, 9)),
	}
	on := model.SearchTuning{WindowBucketDiversify: true}.Normalized()
	assertSameIDs(t, pfIDs(applyPostRerank(model.SearchQuery{}, in, nil, on)), pfIDs(in))
	oneDay := model.SearchQuery{OccurredFrom: pfAt(1, 0), OccurredTo: pfAt(2, 0)}
	assertSameIDs(t, pfIDs(applyPostRerank(oneDay, in, nil, on)), pfIDs(in))
	month := model.SearchQuery{OccurredFrom: pfAt(1, 0), OccurredTo: pfAt(30, 0)}
	assertSameIDs(t, pfIDs(applyPostRerank(month, in, nil, on)), []uuid.UUID{pfID(1), pfID(3), pfID(2)})
	month.Sort = model.SortRecent
	assertSameIDs(t, pfIDs(applyPostRerank(month, in, nil, on)), pfIDs(in))
}

// --- MMR ---

func pfVecDoc(n int, st model.SourceType, score float64, vec ...float32) *model.SearchResult {
	r := pfDoc(n, st, score, nil)
	r.Embedding = vec
	return r
}

func TestApplyMMR_DemotesSameSourceNearDuplicate(t *testing.T) {
	t.Parallel()
	in := []*model.SearchResult{
		pfVecDoc(1, model.SourceCall, 1.0, 1, 0),
		pfVecDoc(2, model.SourceCall, 0.95, 1, 0.01), // 1번의 거의 복제
		pfVecDoc(3, model.SourceCall, 0.9, 0, 1),
	}
	assertSameIDs(t, pfIDs(applyMMR(in, 0.5)), []uuid.UUID{pfID(1), pfID(3), pfID(2)})
	// λ=1 은 순수 관련도: 점수 순 입력은 그대로.
	assertSameIDs(t, pfIDs(applyMMR(in, 1)), pfIDs(in))
	// 무효 λ 는 아무것도 하지 않는다.
	for _, l := range []float64{0, -0.5, 1.5, math.NaN()} {
		assertSameIDs(t, pfIDs(applyMMR(in, l)), pfIDs(in))
	}
	// 입력을 바꾸지 않는다.
	assertSameIDs(t, pfIDs(in), []uuid.UUID{pfID(1), pfID(2), pfID(3)})
}

func TestApplyMMR_NoPenaltyAcrossSources(t *testing.T) {
	t.Parallel()
	in := []*model.SearchResult{
		pfVecDoc(1, model.SourceCall, 1.0, 1, 0),
		pfVecDoc(2, model.SourceGmail, 0.95, 1, 0), // 같은 벡터지만 다른 소스
		pfVecDoc(3, model.SourceCall, 0.9, 0, 1),
	}
	assertSameIDs(t, pfIDs(applyMMR(in, 0.5)), pfIDs(in))
}

func TestApplyMMR_DocsWithoutEmbeddingKeepTheirSlot(t *testing.T) {
	t.Parallel()
	noVec := pfDoc(9, model.SourceCall, 0.97, nil)
	nanVec := pfVecDoc(8, model.SourceCall, 0.5, float32(math.NaN()), 1)
	zeroVec := pfVecDoc(7, model.SourceCall, 0.4, 0, 0)
	in := []*model.SearchResult{
		pfVecDoc(1, model.SourceCall, 1.0, 1, 0),
		noVec,
		pfVecDoc(2, model.SourceCall, 0.95, 1, 0.01),
		pfVecDoc(3, model.SourceCall, 0.9, 0, 1),
		nanVec,
		zeroVec,
		pfVecDoc(4, model.SourceCall, 0.3, 1, 0, 0), // 차원이 다르다: 유사도 0
	}
	got := applyMMR(in, 0.5)
	if len(got) != len(in) {
		t.Fatalf("size changed: %d", len(got))
	}
	if got[1] != noVec || got[4] != nanVec || got[5] != zeroVec {
		t.Fatalf("documents without a usable embedding moved: %v", pfIDs(got))
	}
	// 재배치는 임베딩 있는 문서의 자리(0,2,3,6) 안에서만.
	assertSameIDs(t, []uuid.UUID{got[0].ID, got[2].ID, got[3].ID, got[6].ID},
		[]uuid.UUID{pfID(1), pfID(3), pfID(4), pfID(2)})
}

func TestApplyMMR_WindowAndDeterminism(t *testing.T) {
	t.Parallel()
	in := make([]*model.SearchResult, 0, 40)
	for i := 0; i < 40; i++ {
		in = append(in, pfVecDoc(i+1, model.SourceSMS, float64(40-i), 1, float32(i%2)))
	}
	got := applyMMR(in, 0.3)
	assertSameIDs(t, pfIDs(got[MMRWindow:]), pfIDs(in[MMRWindow:]))
	assertSameIDs(t, pfSortedIDs(got), pfSortedIDs(in))

	for seed := int64(1); seed <= 30; seed++ {
		pool := pfRandomPool(seed, 35)
		a := applyMMR(sortedCopy(pool), 0.6)
		b := applyMMR(sortedCopy(pfShuffled(pool, seed)), 0.6)
		assertSameIDs(t, pfIDs(a), pfIDs(b))
		assertSameIDs(t, pfSortedIDs(a), pfSortedIDs(pool))
	}
	// 빈 입력·단건.
	if len(applyMMR(nil, 0.5)) != 0 || len(applyMMR(in[:1], 0.5)) != 1 {
		t.Fatal("edge sizes changed")
	}
	// NaN 점수가 섞여도 패닉 없이 크기 보존.
	nan := []*model.SearchResult{
		pfVecDoc(1, model.SourceSMS, math.NaN(), 1, 0),
		pfVecDoc(2, model.SourceSMS, 1, 1, 0),
		pfVecDoc(3, model.SourceSMS, math.Inf(1), 0, 1),
	}
	if len(applyMMR(nan, 0.5)) != 3 {
		t.Fatal("NaN scores changed the size")
	}
}

// --- 리랭커 YAML 입력 ---

func TestRerankYAMLDoc(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 1, 16, 0, 0, 0, time.UTC) // KST 09-02 01:00
	r := pfDoc(1, model.SourceCall, 1, &at)
	r.Title = `9월 "정기" 회의`
	r.SourceID = "call-log:1:secrethash:d"
	r.Metadata = map[string]any{
		"contact_name": " 홍길동\n",
		"number":       "010-1234-5678",
		"number_hash":  "secrethash",
	}
	got := rerankYAMLDoc(r, "첫 줄\r\n# 둘째 줄: 콜론\n")
	want := "source: call\n" +
		"date: 2026-09-02\n" +
		"title: \"9월 \\\"정기\\\" 회의\"\n" +
		"counterpart: \"홍길동\"\n" +
		"text: |-\n  첫 줄\n  # 둘째 줄: 콜론"
	if got != want {
		t.Fatalf("yaml doc\n got: %q\nwant: %q", got, want)
	}
	for _, leak := range []string{"010", "1234", "secrethash"} {
		if strings.Contains(got, leak) {
			t.Fatalf("yaml doc leaked %q", leak)
		}
	}

	// 번호처럼 보이는 표시 이름, 날짜·제목 없음, 빈 본문, 알 수 없는 소스.
	bare := pfDoc(2, "", 1, nil)
	bare.Metadata = map[string]any{"contact_name": "+82 (10) 1234-5678"}
	if got := rerankYAMLDoc(bare, " \n"); got != "source: document\ntext: \"\"" {
		t.Fatalf("bare yaml doc = %q", got)
	}
	if looksLikePhoneNumber("홍길동") || !looksLikePhoneNumber("010.1234.5678") || looksLikePhoneNumber("12") {
		t.Fatal("looksLikePhoneNumber misclassified")
	}
}

func TestBuildRerankDocs_YAMLUsesBestChunkBody(t *testing.T) {
	t.Parallel()
	results, chunks := batchFixture()
	best := model.SearchTuning{RerankInput: model.RerankInputBestChunk}.Normalized()
	yaml := model.SearchTuning{RerankInput: model.RerankInputYAML, RerankCallContext: true}.Normalized()
	if yaml.RerankCallContext {
		t.Fatal("Normalized kept RerankCallContext with yaml input")
	}
	svc := &Service{chunkStore: &batchListerDouble{perDocListerDouble: perDocListerDouble{byDoc: chunks}}}
	for _, query := range []string{"예산 승인", "예산", ""} {
		bc := svc.buildRerankDocs(context.Background(), query, results, best)
		ym := svc.buildRerankDocs(context.Background(), query, results, yaml)
		if len(bc) != len(ym) {
			t.Fatalf("doc count differs")
		}
		for i := range bc {
			_, body, _ := strings.Cut(bc[i], "\n") // best_chunk: 머리글 한 줄 + 본문
			_, text, ok := strings.Cut(ym[i], "text: |-\n")
			if !ok {
				t.Fatalf("docs[%d] has no text block: %q", i, ym[i])
			}
			text = strings.ReplaceAll(strings.TrimPrefix(text, "  "), "\n  ", "\n")
			if text != body {
				t.Errorf("query %q docs[%d]: yaml text %q != best_chunk body %q", query, i, text, body)
			}
		}
	}
	// 예산 상한은 같다.
	long := pfDoc(1, model.SourceNote, 1, nil)
	long.Content = strings.Repeat("가", 5000)
	docs := (&Service{}).buildRerankDocs(context.Background(), "q", []*model.SearchResult{long}, yaml)
	if n := len([]rune(docs[0])); n != maxRerankDocRunes {
		t.Fatalf("yaml doc runes = %d, want %d", n, maxRerankDocRunes)
	}
}

// --- 파이프라인: 노브 꺼짐 동일성 ---

// pfStore 는 호출된 질의를 기록하고 고정 결과를 돌려준다. 소스 포함 집합이
// 있으면 그 소스 문서만 돌려준다(운영 저장소의 SQL 필터 흉내).
type pfStore struct {
	mu      sync.Mutex // 보조 검색이 동시에 부른다
	results []*model.SearchResult
	calls   []model.SearchQuery
}

func (s *pfStore) Search(_ context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, q)
	s.mu.Unlock()
	inc := q.IncludeSourceTypes()
	out := make([]*model.SearchResult, 0, len(s.results))
	for _, r := range s.results {
		if len(inc) > 0 && !slices.Contains(inc, r.SourceType) {
			continue
		}
		cp := *r
		out = append(out, &cp)
		if len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

// pfFixture 는 노브 하나하나가 켜지면 결과가 달라지도록 고른 후보다
// (점수 내림차순): 같은 상대·같은 날 문자 6건(접기)이 서로 다른 날의 메일
// 6건(날짜 버킷)과 섞여 있고, 문자끼리·메일끼리 벡터가 같으며(MMR), 상위
// 밖에 일정 문서 하나(보조 검색)가 있다.
//
//	sms1 100, sms2 99, gm1 98, sms3 97, gm2 96, sms4 95, gm3 94,
//	sms5 93, gm4 92, sms6 91, gm5 90, gm6 89, cal 1
func pfFixture() []*model.SearchResult {
	smsScores := []float64{100, 99, 97, 95, 93, 91}
	gmScores := []float64{98, 96, 94, 92, 90, 89}
	var out []*model.SearchResult
	for i, sc := range smsScores {
		r := pfSMS(i+1, "aaaa", sc, pfAt(10, 9+i))
		r.Embedding = []float32{1, 0}
		out = append(out, r)
	}
	for i, sc := range gmScores {
		r := pfDoc(10+i, model.SourceGmail, sc, pfAt(11+i, 9))
		r.Embedding = []float32{0, 1}
		out = append(out, r)
	}
	out = append(out, pfDoc(30, model.SourceCalendar, 1, pfAt(20, 9)))
	sortByScore(out)
	return out
}

func TestPostFusion_KnobsOffIdentity(t *testing.T) {
	t.Parallel()
	window := model.SearchQuery{Query: "q", Limit: 5, OccurredFrom: pfAt(1, 0), OccurredTo: pfAt(30, 0)}
	expected := pfIDs(pfFixture()[:5])

	run := func(q model.SearchQuery, tune model.SearchTuning) ([]uuid.UUID, *pfStore) {
		st := &pfStore{results: pfFixture()}
		svc := NewService(st, disabledEmbedderForTrace{}).WithTuning(model.SearchTuning{})
		q.Tuning = tune
		got, err := svc.Search(context.Background(), q)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		return pfIDs(got), st
	}

	got, st := run(window, model.SearchTuning{})
	assertSameIDs(t, got, expected)
	if len(st.calls) != 1 || st.calls[0].Limit != 5 || len(st.calls[0].IncludeSourceTypes()) != 0 {
		t.Fatalf("knobs off changed the store calls: %d calls", len(st.calls))
	}

	// 양성 대조군: 같은 fixture 에서 노브를 켜면 결과나 저장소 호출이 바뀐다.
	// 대조군이 없으면 위 동일성 검사는 "원래 아무 것도 안 바뀌는 fixture" 로도
	// 통과한다.
	for name, tune := range map[string]model.SearchTuning{
		"collapse": {CollapseContactDay: true},
		"bucket":   {WindowBucketDiversify: true},
		"mmr":      {MMRLambda: 0.3},
	} {
		if got, _ := run(window, tune); slices.Equal(got, expected) {
			t.Errorf("%s: fixture is not sensitive to the knob", name)
		}
	}
	got, st = run(window, model.SearchTuning{SourceStratifyK: 2})
	if len(st.calls) != 1+len(stratifySources) || !slices.Contains(got, pfID(30)) {
		t.Errorf("stratify: calls=%d, calendar doc surfaced=%v", len(st.calls), slices.Contains(got, pfID(30)))
	}

	// 노브가 켜져 있어도 적용 조건이 아니면 동일하다.
	recent := window
	recent.Sort = model.SortRecent
	baseRecent, _ := run(recent, model.SearchTuning{})
	for name, tune := range map[string]model.SearchTuning{
		"collapse": {CollapseContactDay: true},
		"bucket":   {WindowBucketDiversify: true},
		"mmr":      {MMRLambda: 0.3},
	} {
		if got, _ := run(recent, tune); !slices.Equal(got, baseRecent) {
			t.Errorf("%s changed a Sort=recent query", name)
		}
	}
	noWindow := model.SearchQuery{Query: "q", Limit: 5}
	if got, _ := run(noWindow, model.SearchTuning{WindowBucketDiversify: true}); !slices.Equal(got, expected) {
		t.Error("bucket diversify changed a query without a window")
	}
	withInclude := noWindow
	withInclude.SourceTypes = []model.SourceType{model.SourceSMS}
	if _, st := run(withInclude, model.SearchTuning{SourceStratifyK: 3}); len(st.calls) != 1 {
		t.Errorf("stratify ran despite an explicit include set: %d calls", len(st.calls))
	}
}

// 접기는 상위 페이지 자리를 다른 상대에게 열지만, 후보 풀(PoolIDs)에는
// 모든 후보가 남는다.
func TestPostFusion_CollapseKeepsEveryCandidateInPool(t *testing.T) {
	t.Parallel()
	st := &pfStore{results: pfFixture()}
	svc := NewService(st, disabledEmbedderForTrace{}).WithReranker(traceReranker{})
	q := model.SearchQuery{Query: "q", Limit: 7, UseRerank: true, // 오버페치 14 ≥ 13건
		Tuning: model.SearchTuning{CollapseContactDay: true, CollapseExpandMax: 1}}
	got, trace, err := svc.SearchTraced(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 {
		t.Fatalf("page size = %d", len(got))
	}
	all := pfFixture()
	assertSameIDs(t, pfSortIDs(trace.PoolIDs), pfSortedIDs(all))
	// 리랭커는 접힌 뒤의 대표 목록만 받는다: 문자 6건 → 대표 1건.
	if len(trace.PreRerankIDs) != len(all)-5 {
		t.Fatalf("reranker saw %d docs, want %d", len(trace.PreRerankIDs), len(all)-5)
	}
	// traceReranker 는 순서를 뒤집으므로 대표 sms1 이 대표 목록의 마지막이다.
	// 그 바로 뒤에 펼침 1건(sms2), 맨 끝에 초과분 sms3..sms6 이 점수 순으로 온다.
	pool := trace.PoolIDs
	n := len(pool)
	assertSameIDs(t, pool[n-6:], []uuid.UUID{pfID(1), pfID(2), pfID(3), pfID(4), pfID(5), pfID(6)})
}

// 리랭크 없는 실행에서 보조 검색 합류 결과가 저장소 정렬과 무관하게
// 결정론적인지(같은 질의 두 번) 확인한다.
func TestPostFusion_StratifyDeterministic(t *testing.T) {
	t.Parallel()
	var first []uuid.UUID
	for i := 0; i < 5; i++ {
		st := &pfStore{results: pfFixture()}
		got, err := NewService(st, disabledEmbedderForTrace{}).Search(context.Background(),
			model.SearchQuery{Query: "q", Limit: 8, Tuning: model.SearchTuning{SourceStratifyK: 3}})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = pfIDs(got)
			continue
		}
		assertSameIDs(t, pfIDs(got), first)
	}
}

// 회귀: 리랭커가 상위 N건만 음수(로짓) 점수로 돌려주면 applyRerank 는 나머지를
// 점수 0 으로 채워 뒤에 붙인다. MMR 관련도를 원점수 min-max 로 정하면 그
// 0점 꼬리가 최고 관련도가 되어 리랭크된 문서 위로 올라온다. 순위 기반
// 관련도에서는 꼬리가 자기 자리(리랭크된 문서 뒤)를 벗어나지 않아야 한다.
func TestApplyMMR_ZeroFilledTailAfterNegativeRerankScoresStaysBehind(t *testing.T) {
	t.Parallel()
	// 리랭크된 3건은 서로 직교, 꼬리 3건은 서로 같은 벡터(서로 벌점)이고
	// 리랭크된 문서와 직교한다. 7번째 후보는 임베딩이 없다.
	vecs := [][]float32{{1, 0, 0, 0}, {0, 1, 0, 0}, {0, 0, 1, 0}, {0, 0, 0, 1}, {0, 0, 0, 1}, {0, 0, 0, 1}, nil}
	in := make([]*model.SearchResult, len(vecs))
	for i, v := range vecs {
		in[i] = pfDoc(i+1, model.SourceCall, float64(len(vecs)-i), nil)
		in[i].Embedding = v
	}
	svc := newServiceWithReranker(&mockReranker{enabled: true,
		fn: func(context.Context, string, []string) ([]RerankResult, error) {
			// top_n=3, 로짓 점수(음수). 나머지 4건은 applyRerank 가 0 으로 채운다.
			return []RerankResult{{Index: 0, Score: -1}, {Index: 1, Score: -2}, {Index: 2, Score: -3}}, nil
		}})
	reranked, err := svc.applyRerank(context.Background(), "q", in, model.SearchTuning{})
	if err != nil {
		t.Fatal(err)
	}
	if reranked[3].Score != 0 || reranked[0].Score != -1 {
		t.Fatalf("fixture premise broken: scores %v", []float64{reranked[0].Score, reranked[3].Score})
	}
	for _, lambda := range []float64{0.1, 0.3, 0.5, 0.7, 1} {
		got := applyMMR(reranked, lambda)
		head := []uuid.UUID{got[0].ID, got[1].ID, got[2].ID}
		if !slices.Equal(pfSortIDs(head), []uuid.UUID{pfID(1), pfID(2), pfID(3)}) {
			t.Fatalf("λ=%v: zero-filled tail rose above reranked docs: %v", lambda, pfIDs(got))
		}
		if got[6].ID != pfID(7) {
			t.Fatalf("λ=%v: document without embedding moved: %v", lambda, pfIDs(got))
		}
		assertSameIDs(t, pfSortedIDs(got), pfSortedIDs(in))
	}
}
