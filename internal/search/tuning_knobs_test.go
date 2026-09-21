package search

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// tuneResult 는 노브 테스트용 결과 한 건을 만든다. 기존 makeSearchResult 와
// 달리 사건 시각·소스·본문을 직접 지정할 수 있어야 최신성 감쇠와 리랭커
// 입력을 검증할 수 있다.
func tuneResult(id uuid.UUID, title, content string, score float64) *model.SearchResult {
	return &model.SearchResult{
		Document: model.Document{ID: id, Title: title, Content: content},
		Score:    score,
	}
}

func uuidN(n byte) uuid.UUID {
	var id uuid.UUID
	id[15] = n
	return id
}

func TestSearchTuningNormalized(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   model.SearchTuning
		want model.SearchTuning
	}{
		{
			name: "제로값은 현행 동작으로 채워진다",
			in:   model.SearchTuning{},
			want: model.SearchTuning{
				MergeMode: model.MergeAsymmetric, RerankBlend: model.RerankBlendReplace,
				RerankBlendWeight: 1.0, RerankInput: model.RerankInputHead, RecencyAlpha: 0.3,
			},
		},
		{
			name: "알 수 없는 문자열은 기본값으로 되돌린다",
			in: model.SearchTuning{
				MergeMode: "SYMMETRIC-오타", RerankBlend: "blend", RerankInput: "chunk",
			},
			want: model.SearchTuning{
				MergeMode: model.MergeAsymmetric, RerankBlend: model.RerankBlendReplace,
				RerankBlendWeight: 1.0, RerankInput: model.RerankInputHead, RecencyAlpha: 0.3,
			},
		},
		{
			name: "허용된 값은 보존된다",
			in: model.SearchTuning{
				RerankOverfetch: 50, MergeMode: model.MergeSymmetric,
				RerankBlend: model.RerankBlendRRF, RerankBlendWeight: 0.5,
				RerankInput: model.RerankInputBestChunk, RecencyHalfLifeDays: 30, RecencyAlpha: 0.7,
			},
			want: model.SearchTuning{
				RerankOverfetch: 50, MergeMode: model.MergeSymmetric,
				RerankBlend: model.RerankBlendRRF, RerankBlendWeight: 0.5,
				RerankInput: model.RerankInputBestChunk, RecencyHalfLifeDays: 30, RecencyAlpha: 0.7,
			},
		},
		{
			name: "음수와 범위 밖 값은 안전한 값으로 좁힌다",
			in: model.SearchTuning{
				RerankOverfetch: -5, RerankBlendWeight: -1, RecencyHalfLifeDays: -30, RecencyAlpha: 9,
			},
			want: model.SearchTuning{
				MergeMode: model.MergeAsymmetric, RerankBlend: model.RerankBlendReplace,
				RerankBlendWeight: 1.0, RerankInput: model.RerankInputHead, RecencyAlpha: 1,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.Normalized(); got != tc.want {
				t.Errorf("Normalized() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRerankPoolLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		base, overfetch int
		want            int
	}{
		{"기본값 0 은 현행 풀을 그대로 쓴다", 20, 0, 20},
		{"하한이 현행보다 크면 하한을 쓴다", 20, 50, 50},
		{"하한이 현행보다 작으면 현행을 쓴다", 100, 50, 100},
		{"상한(200)을 넘겨 요청해도 상한에서 막힌다", 20, 5000, overfetchLimitCap},
		{"음수는 무시한다", 20, -1, 20},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := rerankPoolLimit(tc.base, tc.overfetch); got != tc.want {
				t.Errorf("rerankPoolLimit(%d, %d) = %d, want %d", tc.base, tc.overfetch, got, tc.want)
			}
		})
	}
}

// TestMergeRRFMode_SymmetricAdmitsSecondaryOnlyHit 은 양성 대조군이다.
// 정확히 같은 입력이 기본(asymmetric)에서는 secondary 단독 히트를 막고,
// symmetric 에서는 들여보낸다 — 노브가 실제로 무언가를 바꾼다는 증거다.
//
// 실측 배경: 판정 정답 1건이 chunk_vector 레인에 분명히 잡혔는데도 후보에
// 없었다. primary 가 풀을 이미 채워 비대칭 규칙이 진입 자체를 막았기 때문이다.
func TestMergeRRFMode_SymmetricAdmitsSecondaryOnlyHit(t *testing.T) {
	t.Parallel()

	p1, p2, p3 := uuidN(1), uuidN(2), uuidN(3)
	s1 := uuidN(9)
	primary := []*model.SearchResult{
		tuneResult(p1, "primary 1", "", 0.9),
		tuneResult(p2, "primary 2", "", 0.8),
		tuneResult(p3, "primary 3", "", 0.7),
	}
	secondary := []*model.SearchResult{tuneResult(s1, "chunk-only hit", "", 0.95)}

	contains := func(rs []*model.SearchResult, id uuid.UUID) bool {
		for _, r := range rs {
			if r.ID == id {
				return true
			}
		}
		return false
	}

	asym := mergeRRFMode(primary, secondary, 3, model.MergeAsymmetric)
	if contains(asym, s1) {
		t.Errorf("asymmetric(기본)에서 secondary 단독 히트가 들어왔다; 기존 회귀 방어가 깨졌다: %+v", asym)
	}

	sym := mergeRRFMode(primary, secondary, 3, model.MergeSymmetric)
	if !contains(sym, s1) {
		t.Errorf("symmetric 에서 secondary 단독 히트가 여전히 막혔다; 노브가 동작하지 않는다: %+v", sym)
	}
	if len(sym) != 3 {
		t.Errorf("symmetric 도 페이지 절단은 그대로여야 한다: len=%d", len(sym))
	}
	// RRF 산술: primary 1위(p1)와 secondary 1위(s1)는 둘 다 1/61 로 동률이고,
	// primary 3위(p3)는 1/63 이다. 따라서 s1 은 상위 2위 안에 들어가고 p3 가
	// 페이지 밖으로 밀려난다. 동률인 p1/s1 의 선후는 sortByScore 의 결정론적
	// 동률 처리에 맡기므로 "s1 이 반드시 1위"라고 단정하지 않는다 — 슬롯이
	// 아니라 점수로 갈린다는 것이 이 모드의 정의이고, 그 증거는 p3 축출이다.
	if contains(sym, p3) {
		t.Errorf("symmetric 에서 점수가 낮은 primary 3위(1/63)가 secondary 1위(1/61)에 밀려나야 한다: %+v", sym)
	}
	if sym[0].ID != s1 && sym[1].ID != s1 {
		t.Errorf("symmetric 은 RRF 점수로 정렬해야 한다; s1(1/61)은 상위 2위 안이어야 함, 순서=%s,%s,%s", sym[0].ID, sym[1].ID, sym[2].ID)
	}
}

// TestMergeRRF_DefaultStaysAsymmetric 은 인자 3개짜리 기존 호출부가 여전히
// 비대칭 동작을 한다는 것을 고정한다. mergeRRF 를 부르는 기존 테스트 5건이
// 이 보장 위에 서 있다.
func TestMergeRRF_DefaultStaysAsymmetric(t *testing.T) {
	t.Parallel()

	primary := []*model.SearchResult{tuneResult(uuidN(1), "p", "", 0.9)}
	secondary := []*model.SearchResult{tuneResult(uuidN(2), "s", "", 0.9)}

	got := mergeRRF(primary, secondary, 1)
	if len(got) != 1 || got[0].ID != uuidN(1) {
		t.Errorf("mergeRRF 기본값이 비대칭이 아니다: %+v", got)
	}
}

// TestBlendRerankRRF 는 리랭커가 순서를 완전히 뒤집었을 때 합산이 상위권을
// 지키는지 본다. 양성 대조군: 같은 입력에서 replace 는 뒤집힌 순서를,
// rrf 는 1위를 지킨 순서를 낸다.
func TestBlendRerankRRF(t *testing.T) {
	t.Parallel()

	ids := []uuid.UUID{uuidN(1), uuidN(2), uuidN(3)}
	fused := []*model.SearchResult{
		tuneResult(ids[0], "융합 1위(정답)", "", 0.9),
		tuneResult(ids[1], "융합 2위", "", 0.8),
		tuneResult(ids[2], "융합 3위", "", 0.7),
	}
	// 리랭커가 정답을 꼴찌로 내린 상황 — 실측에서 통화 정답 5건에 일어난 일이다.
	reranked := []*model.SearchResult{
		tuneResult(ids[2], "융합 3위", "", 0.99),
		tuneResult(ids[1], "융합 2위", "", 0.98),
		tuneResult(ids[0], "융합 1위(정답)", "", 0.01),
	}

	got := blendRerankRRF(fused, reranked, model.DefaultRerankBlendWeight)
	if len(got) != 3 {
		t.Fatalf("합산이 문서를 잃었다: %d", len(got))
	}
	// 정답: 1/(60+1) + 1/(60+3) = 0.03226. 리랭커 1위: 1/(60+3) + 1/(60+1) — 동일.
	// 가중치 1.0 에서 두 항이 대칭이므로 동점이며, 확인할 것은 "리랭커가 꼴찌로
	// 내린 문서가 여전히 상위 2위 안에 있다" 는 사실이다. replace 였다면 3위다.
	rank := map[uuid.UUID]int{}
	for i, r := range got {
		rank[r.ID] = i + 1
	}
	if rank[ids[0]] > 2 {
		t.Errorf("합산에서도 정답이 %d위로 밀렸다; 융합 순위 항이 듣지 않는다: %+v", rank[ids[0]], rank)
	}

	// 가중치를 키우면 리랭커 쪽으로 기운다 — 노브가 연속적으로 동작한다는 증거.
	heavy := blendRerankRRF(fused, reranked, 5.0)
	if heavy[0].ID != ids[2] {
		t.Errorf("가중치 5.0 이면 리랭커 1위가 최종 1위여야 한다; got %s", heavy[0].ID)
	}
}

func TestBlendRerankRRF_EmptyRerankedKeepsFusedOrder(t *testing.T) {
	t.Parallel()

	fused := []*model.SearchResult{tuneResult(uuidN(1), "a", "", 1)}
	if got := blendRerankRRF(fused, nil, 1.0); len(got) != 1 || got[0].ID != uuidN(1) {
		t.Errorf("리랭커 결과가 비면 융합 순서를 그대로 돌려줘야 한다: %+v", got)
	}
}

func TestApplyRecencyDecay(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -60)
	recent := now.AddDate(0, 0, -1)
	future := now.AddDate(0, 0, 7)
	tune := model.SearchTuning{RecencyHalfLifeDays: 30, RecencyAlpha: 0.3}.Normalized()

	tests := []struct {
		name      string
		query     model.SearchQuery
		tune      model.SearchTuning
		occurred  *time.Time
		wantScore float64
	}{
		{
			name: "반감기 0(기본)은 점수를 건드리지 않는다",
			tune: model.SearchTuning{}.Normalized(), occurred: &old, wantScore: 1.0,
		},
		{
			name: "occurred_at 이 없으면 감쇠하지 않는다",
			tune: tune, occurred: nil, wantScore: 1.0,
		},
		{
			name: "미래 문서는 감쇠하지 않는다",
			tune: tune, occurred: &future, wantScore: 1.0,
		},
		{
			// 60일 = 반감기 2회 → decay=0.25 → 승수 0.7 + 0.3*0.25 = 0.775
			name: "오래된 문서는 alpha 만큼만 감쇠한다",
			tune: tune, occurred: &old, wantScore: 0.775,
		},
		{
			name:  "시간창이 있는 질의에는 적용하지 않는다",
			query: model.SearchQuery{OccurredFrom: &old},
			tune:  tune, occurred: &old, wantScore: 1.0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := tuneResult(uuidN(1), "doc", "", 1.0)
			in.OccurredAt = tc.occurred
			got := applyRecencyDecay(tc.query, []*model.SearchResult{in}, tc.tune, now)
			if len(got) != 1 {
				t.Fatalf("결과 개수가 바뀌었다: %d", len(got))
			}
			if diff := got[0].Score - tc.wantScore; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("Score = %v, want %v", got[0].Score, tc.wantScore)
			}
			if in.Score != 1.0 {
				t.Errorf("입력 슬라이스를 변형했다: %v", in.Score)
			}
		})
	}

	// 양성 대조군: 감쇠가 실제로 순서를 바꾼다.
	t.Run("최신 문서가 오래된 문서를 역전한다", func(t *testing.T) {
		t.Parallel()
		oldDoc := tuneResult(uuidN(1), "오래된 1위", "", 1.0)
		oldDoc.OccurredAt = &old
		newDoc := tuneResult(uuidN(2), "최근 2위", "", 0.85)
		newDoc.OccurredAt = &recent
		in := []*model.SearchResult{oldDoc, newDoc}

		if off := applyRecencyDecay(model.SearchQuery{}, in, model.SearchTuning{}.Normalized(), now); off[0].ID != uuidN(1) {
			t.Fatalf("감쇠를 끄면 순서가 그대로여야 한다: %s", off[0].ID)
		}
		on := applyRecencyDecay(model.SearchQuery{}, in, tune, now)
		if on[0].ID != uuidN(2) {
			t.Errorf("감쇠를 켜면 최근 문서가 1위여야 한다; got %s (%v vs %v)",
				on[0].ID, on[0].Score, on[1].Score)
		}
	})
}

// chunkListerDouble 은 ChunkSearcher 와 ChunkLister 를 동시에 만족하는 더블이다.
// Service.chunkLister() 가 s.chunkStore 에 대한 인터페이스 단언으로 조회기를
// 찾으므로 둘 다 필요하다.
type chunkListerDouble struct {
	chunks []store.Chunk
	err    error
	calls  int
}

func (d *chunkListerDouble) SearchFTS(context.Context, string, int) ([]store.ChunkSearchResult, error) {
	return nil, nil
}

func (d *chunkListerDouble) SearchVector(context.Context, []float32, int) ([]store.ChunkSearchResult, error) {
	return nil, nil
}

func (d *chunkListerDouble) ListByDocument(context.Context, uuid.UUID) ([]store.Chunk, error) {
	d.calls++
	return d.chunks, d.err
}

func TestBuildRerankDocs(t *testing.T) {
	t.Parallel()

	occurred := time.Date(2026, 9, 1, 13, 30, 0, 0, time.UTC)
	doc := tuneResult(uuidN(1), "9월 정기 회의", "인사말과 서명뿐인 앞부분", 1.0)
	doc.SourceType = model.SourceCall
	doc.OccurredAt = &occurred

	t.Run("head(기본)는 제목+본문 앞부분을 보낸다", func(t *testing.T) {
		t.Parallel()
		lister := &chunkListerDouble{chunks: []store.Chunk{{ChunkIndex: 0, Content: "예산 승인 이야기"}}}
		svc := &Service{chunkStore: lister}
		got := svc.buildRerankDocs(context.Background(), "예산 승인",
			[]*model.SearchResult{doc}, model.SearchTuning{}.Normalized())
		if got[0] != "9월 정기 회의\n인사말과 서명뿐인 앞부분" {
			t.Errorf("head 입력이 현행과 다르다: %q", got[0])
		}
		if lister.calls != 0 {
			t.Errorf("head 모드는 DB 를 읽지 않아야 한다: %d회 호출", lister.calls)
		}
	})

	t.Run("best_chunk 는 머리글과 가장 잘 맞는 청크를 보낸다", func(t *testing.T) {
		t.Parallel()
		lister := &chunkListerDouble{chunks: []store.Chunk{
			{ChunkIndex: 0, Content: "안녕하세요 오랜만입니다"},
			{ChunkIndex: 1, Content: "예산 승인 건은 다음 주에 처리하기로 했습니다"},
			{ChunkIndex: 2, Content: "그럼 들어가세요"},
		}}
		svc := &Service{chunkStore: lister}
		tune := model.SearchTuning{RerankInput: model.RerankInputBestChunk}.Normalized()
		got := svc.buildRerankDocs(context.Background(), "예산 승인", []*model.SearchResult{doc}, tune)

		if !strings.HasPrefix(got[0], "[통화 · 2026-09-01 · 9월 정기 회의]\n") {
			t.Errorf("머리글이 기대와 다르다: %q", got[0])
		}
		if !strings.Contains(got[0], "예산 승인 건은") {
			t.Errorf("질의와 맞는 청크가 선택되지 않았다: %q", got[0])
		}
		if lister.calls != 1 {
			t.Errorf("DB 왕복은 문서당 1회여야 한다: %d회", lister.calls)
		}
	})

	t.Run("청크 조회가 실패하면 head 본문으로 되돌아간다", func(t *testing.T) {
		t.Parallel()
		lister := &chunkListerDouble{err: errors.New("boom")}
		svc := &Service{chunkStore: lister}
		tune := model.SearchTuning{RerankInput: model.RerankInputBestChunk}.Normalized()
		got := svc.buildRerankDocs(context.Background(), "예산 승인", []*model.SearchResult{doc}, tune)
		if !strings.Contains(got[0], "인사말과 서명뿐인 앞부분") {
			t.Errorf("폴백이 동작하지 않았다: %q", got[0])
		}
	})

	t.Run("청크 레인 결과는 DB 를 다시 읽지 않는다", func(t *testing.T) {
		t.Parallel()
		lister := &chunkListerDouble{chunks: []store.Chunk{{Content: "쓰이면 안 되는 청크"}}}
		svc := &Service{chunkStore: lister}
		chunkHit := tuneResult(uuidN(2), "긴 메일", "이미 고른 청크 본문", 1.0)
		chunkHit.MatchType = "chunk-vector"
		tune := model.SearchTuning{RerankInput: model.RerankInputBestChunk}.Normalized()
		got := svc.buildRerankDocs(context.Background(), "질의", []*model.SearchResult{chunkHit}, tune)
		if !strings.Contains(got[0], "이미 고른 청크 본문") || lister.calls != 0 {
			t.Errorf("청크 레인 결과를 재조회했다 (calls=%d): %q", lister.calls, got[0])
		}
	})

	t.Run("상한 rune 수는 현행 값을 유지한다", func(t *testing.T) {
		t.Parallel()
		long := tuneResult(uuidN(3), "제목", strings.Repeat("가", 5000), 1.0)
		svc := &Service{}
		tune := model.SearchTuning{RerankInput: model.RerankInputBestChunk}.Normalized()
		got := svc.buildRerankDocs(context.Background(), "질의", []*model.SearchResult{long}, tune)
		if n := len([]rune(got[0])); n != maxRerankDocRunes {
			t.Errorf("rune 상한이 바뀌었다: %d, want %d", n, maxRerankDocRunes)
		}
	})
}

func TestRerankHeader(t *testing.T) {
	t.Parallel()

	occurred := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		in   *model.SearchResult
		want string
	}{
		{
			name: "소스·날짜·제목",
			in: func() *model.SearchResult {
				r := tuneResult(uuidN(1), "제목", "", 0)
				r.SourceType = model.SourceSMS
				r.OccurredAt = &occurred
				return r
			}(),
			want: "[문자 · 2026-01-02 · 제목]",
		},
		{
			name: "사건 시각이 없으면 날짜를 생략한다",
			in: func() *model.SearchResult {
				r := tuneResult(uuidN(1), "제목", "", 0)
				r.SourceType = model.SourceGmail
				return r
			}(),
			want: "[메일 · 제목]",
		},
		{
			name: "모르는 소스는 원래 값을 쓴다",
			in: func() *model.SearchResult {
				r := tuneResult(uuidN(1), "", "", 0)
				r.SourceType = model.SourceType("brand-new")
				return r
			}(),
			want: "[brand-new]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := rerankHeader(tc.in); got != tc.want {
				t.Errorf("rerankHeader() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveTuning_RequestWinsOverService(t *testing.T) {
	t.Parallel()

	svc := &Service{tuning: model.SearchTuning{RerankOverfetch: 10}}
	if got := svc.resolveTuning(model.SearchQuery{}); got.RerankOverfetch != 10 {
		t.Errorf("요청이 비면 서비스 기본값을 써야 한다: %+v", got)
	}
	q := model.SearchQuery{Tuning: model.SearchTuning{RerankOverfetch: 50}}
	if got := svc.resolveTuning(q); got.RerankOverfetch != 50 {
		t.Errorf("요청 값이 서비스 기본값을 이겨야 한다: %+v", got)
	}
}
