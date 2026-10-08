package model

import (
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
)

// isBadFloat 는 NaN·무한대를 걸러낸다. 이 값들은 비교 연산이 전부 false 라
// 범위 검사만으로는 잡히지 않고, 그대로 점수 계산에 들어가면 정렬 결과가
// 입력 순서에 따라 달라진다.
func isBadFloat(f float64) bool { return math.IsNaN(f) || math.IsInf(f, 0) }

// 검색 튜닝 노브가 받는 값. 문자열이 평가 실행 프로필(config_hash)에도
// 그대로 실리므로 상수로 고정한다 — 오타 하나가 별개의 baseline 계열을
// 만들어 버리면 비교 자체가 성립하지 않는다.
const (
	// MergeAsymmetric 은 mergeRRF 의 현행 동작이다. secondary(청크 벡터 /
	// OpenSearch) 단독 히트는 primary 가 페이지를 다 채우지 못했을 때
	// 남은 슬롯에만 들어간다.
	MergeAsymmetric = "asymmetric"
	// MergeSymmetric 은 secondary 단독 히트도 RRF 점수로 동등하게 경쟁시킨다.
	// 페이지 절단은 여전히 마지막에 일어나므로, 이 모드가 바꾸는 것은
	// "오버페치 풀에 들어올 수 있는가" 이지 "최종 페이지를 차지하는가" 가
	// 아니다.
	MergeSymmetric = "symmetric"

	// RerankBlendReplace 는 현행 동작이다. 최종 순서를 리랭커 순위로 대체한다.
	RerankBlendReplace = "replace"
	// RerankBlendRRF 는 융합 순위와 리랭커 순위를 RRF 로 합산한다.
	// 리랭커가 틀렸을 때 융합 순위가 방파제 역할을 한다.
	RerankBlendRRF = "rrf"

	// RerankInputHead 는 현행 동작이다. 제목+본문 앞부분을 잘라 보낸다.
	RerankInputHead = "head"
	// RerankInputBestChunk 는 문서 메타 한 줄 + 질의와 가장 잘 맞는 청크
	// 본문을 보낸다. 통화 전사·긴 메일처럼 근거가 앞부분에 없는 문서를
	// 겨냥한다.
	RerankInputBestChunk = "best_chunk"

	// ChunkSparseFallback 은 현행 동작이다(#270). 청크 FTS/bigm 레인은
	// 1차 경로(문서 하이브리드 + 청크 벡터 + OpenSearch)가 결과를 하나도
	// 못 찾았을 때만 폴백으로 돈다.
	ChunkSparseFallback = "fallback"
	// ChunkSparseFuse 는 청크 FTS/bigm 레인을 1차 경로 결과 유무와 무관하게
	// 항상 RRF 융합에 참여시킨다(실험용). 스키마 변경 없음 — chunks.content
	// 만 매칭·반환한다(#270 phase A).
	ChunkSparseFuse = "fuse"
	// ChunkSparseFuseCtx 는 ChunkSparseFuse 와 같되, chunk_sparse_context
	// (마이그레이션 040)에 미리 계산해 둔 파생 문맥(제목·참여자 헤더 +
	// 청크 본문)을 매칭 대상으로 우선 쓴다. 문서가 갱신돼 파생 문맥이
	// stale 해진 청크는 자동으로 raw 청크 본문 매칭으로 빠진다(#270 phase B).
	ChunkSparseFuseCtx = "fuse_ctx"

	// ChunkSparseCtxV1TP 는 제목+참여자만 담는 sparse-context 레시피다
	// (internal/chunkctx.RecipeTP). 날짜·소스 라벨은 넣지 않는다 — 소스
	// 라벨 토큰("메일", "통화")이 그 단어를 포함한 질의에서 오탐을 키울 수
	// 있어 ablation 조건으로 분리한다.
	ChunkSparseCtxV1TP = "v1-tp"
	// ChunkSparseCtxV1Full 은 임베딩 헤더(BuildChunkContextHeader)와 정확히
	// 같은 구성을 쓴다(internal/chunkctx.RecipeFull).
	//
	// 이 두 상수는 internal/chunkctx 가 아니라 여기 model 패키지에 있다 —
	// internal/chunkctx 가 이미 이 패키지를 임포트하므로(model.Document),
	// 반대 방향 임포트는 순환이 된다.
	ChunkSparseCtxV1Full = "v1-full"

	// SparseQueryRaw 는 현행 동작이다(#276). 희소 레인(FTS·bigm)은 질문
	// 원문을 plainto_tsquery(AND)와 LIKE '%질문 전체%' 로 그대로 쓴다.
	SparseQueryRaw = "raw"
	// SparseQueryChunk 는 청크 희소 레인(청크 FTS 폴백·fuse·fuse_ctx)에만
	// internal/sparseq 가 뽑은 키워드를 쓴다 — 접두 OR tsquery 와 키워드별
	// LIKE. 문서 레인과 엔티티 레인은 그대로다.
	SparseQueryChunk = "chunk"
	// SparseQueryChunkDoc 은 SparseQueryChunk 에 더해 문서 하이브리드의
	// fts·bigm 레인과 임베딩 없는 fulltext 경로에도 키워드를 쓴다. 엔티티
	// 레인은 어떤 값에서도 바뀌지 않는다(방향 문제는 별도 이슈).
	SparseQueryChunkDoc = "chunk_doc"

	// EntityKeywordsOff 는 현행 동작이다. 엔티티 레인은 질문 원문 전체를
	// 소문자로 만들어 normalized_name LIKE '%질문 전체%' 로 맞춘다(실제로는
	// 문장형 질문에서 거의 맞지 않는다).
	EntityKeywordsOff = ""
	// EntityKeywordsSparse 는 internal/sparseq.Extract 가 뽑은 키워드를
	// 저수준(low-level) 키워드로 써서 엔티티 레인을 normalized_name 정확
	// 일치·접두 일치로 맞춘다. LLM 호출이 없고 결정론적이다.
	EntityKeywordsSparse = "sparse"
	// EntityKeywordsLLM 은 LLM 한 번으로 저수준(고유명사)·고수준(주제)
	// 키워드를 뽑는다(LightRAG 의 이중 키워드). 어떤 실패든 EntityKeywordsSparse
	// 결과로 조용히 되돌아간다.
	EntityKeywordsLLM = "llm"

	// RRFMissingRankZero 는 현행 동작이다. 어떤 레인에도 없는 문서는 그 레인에서
	// 0 을 받는다.
	RRFMissingRankZero = ""
	// RRFMissingRankCutoff 는 R2R 의 가중 RRF 처럼, 켜져 있고(가중치>0) 결과가
	// 하나라도 있는 레인에 없는 문서를 "레인 상한 바로 다음 순위"(LIMIT+1)로
	// 본다 — w/(k + lane_limit + 1). 레인 하나에만 잡힌 문서가 여러 레인에서
	// 중간쯤 잡힌 문서를 이기는 일을 줄인다.
	RRFMissingRankCutoff = "cutoff"
)

// 노브 기본값. 제로값이 곧 "현행 동작" 이 되도록 잡았다 — 새 필드가 생겼다는
// 이유만으로 기존 호출자의 검색 결과가 달라지면 안 된다.
const (
	// DefaultRerankBlendWeight 는 RerankBlendRRF 에서 리랭커 순위 항에
	// 곱하는 가중치다. 1.0 이면 융합 순위와 동등하게 본다.
	DefaultRerankBlendWeight = 1.0
	// DefaultRecencyAlpha 는 최신성 감쇠의 최대 강도다. 0.3 이면 아무리
	// 오래된 문서라도 점수가 원래의 70% 밑으로는 내려가지 않는다.
	DefaultRecencyAlpha = 0.3
)

// SearchTuning 은 검색 실험용 노브 묶음이다.
//
// **제로값이 현행 동작이다.** 필드를 하나도 채우지 않은 SearchTuning 은
// 이 타입이 생기기 전의 코드 경로와 완전히 같은 결과를 낸다. 노브는
// 환경변수(EnvSearchTuning)·요청 필드(SearchQuery.Tuning)·평가 플래그
// (cmd/eval) 세 경로로만 켜지며, 켜지 않은 배포는 아무 영향도 받지 않는다.
//
// 실측 배경(사용자 판정 골든셋 28질의): 리랭크를 켜면 ndcg@10 이 0.692 에서
// 0.516 으로 떨어졌고, 정답 문서 24건 중 리랭커가 순위를 올린 건은 0건이었다.
// 원인은 하나가 아니라 넷이 겹쳤다 — 오버페치 풀이 20건뿐이라 정답 7건이
// 애초에 리랭커에 도달하지 못했고, 그중 1건은 청크 레인이 찾았는데
// mergeRRF 의 비대칭 융합에 막혔으며, 리랭커 입력이 본문 앞부분 절단이라
// 통화 전사의 근거가 잘렸고, 최신 편향이 강한 개인 데이터 질의에서 시간
// 신호가 전혀 반영되지 않았다. 각 노브는 그 넷에 1:1로 대응한다.
type SearchTuning struct {
	// RerankOverfetch 는 리랭크가 켜졌을 때 후보 풀 크기의 하한이다.
	// 0(기본)이면 현행 overfetchLimit(limit*2, 상한 200)을 그대로 쓴다.
	// 예: 50 이면 limit=10 에서도 후보 50건이 리랭커에 간다.
	RerankOverfetch int

	// MergeMode 는 MergeAsymmetric(기본) 또는 MergeSymmetric.
	MergeMode string

	// RerankBlend 는 RerankBlendReplace(기본) 또는 RerankBlendRRF.
	RerankBlend string
	// RerankBlendWeight 는 RerankBlendRRF 에서만 쓰인다. 0 이면
	// DefaultRerankBlendWeight.
	RerankBlendWeight float64

	// RerankInput 은 RerankInputHead(기본) 또는 RerankInputBestChunk.
	RerankInput string

	// RecencyHalfLifeDays 는 최신성 감쇠의 반감기(일)다. 0(기본)이면 감쇠를
	// 아예 적용하지 않는다. 시간창(OccurredFrom/OccurredTo)이 있는 질의에는
	// 어떤 값이어도 적용되지 않는다 — 기간을 이미 지정한 질의에 다시 최신을
	// 얹으면 사용자가 고른 기간 안에서 순서가 뒤집힌다.
	RecencyHalfLifeDays float64
	// RecencyAlpha 는 감쇠의 최대 강도다. 0 이면 DefaultRecencyAlpha.
	// 승수는 (1-alpha) + alpha*exp(-ln2*age/halflife) 이므로 alpha=1 이면
	// 반감기마다 점수가 절반이 되고, alpha=0 이면 감쇠가 없다.
	RecencyAlpha float64

	// ChunkSparse 는 ChunkSparseFallback(기본)·ChunkSparseFuse·
	// ChunkSparseFuseCtx 중 하나.
	ChunkSparse string
	// ChunkSparseCtxVersion 은 ChunkSparse==ChunkSparseFuseCtx 일 때만
	// 쓰인다. ChunkSparseCtxV1TP 또는 ChunkSparseCtxV1Full. 그 외 값(또는
	// fuse_ctx 가 아닌데 채워진 값)은 Normalized 가 안전하게 비운다.
	ChunkSparseCtxVersion string

	// SparseQuery 는 SparseQueryRaw(기본)·SparseQueryChunk·
	// SparseQueryChunkDoc 중 하나(#276). 어느 값이든 리랭커·임베딩·엔티티
	// 레인은 질문 원문을 그대로 받는다 — 바뀌는 것은 희소 레인의 매칭
	// 조건뿐이다.
	SparseQuery string

	// EntityQueryContainsName은 질의 안에서 두 글자 이상 엔티티 이름을 찾는다.
	// 기본 false: 골든 평가 후에만 운영 기본값 변경을 검토한다.
	EntityQueryContainsName bool
	// RerankCallContext는 통화 리랭커 입력에 참여자와 본문 앞부분을 보탠다.
	RerankCallContext bool

	// EntityKeywordMode 는 EntityKeywordsOff(기본)·EntityKeywordsSparse·
	// EntityKeywordsLLM 중 하나다. 꺼져 있지 않으면 search.Service 가 질문에서
	// 저수준 키워드를 뽑아 SearchQuery.EntityKeywords 에 싣고, 엔티티 레인과
	// 그래프 레인(GraphWeight)이 그 키워드로 엔티티를 찾는다. 키워드가 하나도
	// 안 나온 질의는 현행 엔티티 레인 SQL 을 그대로 탄다.
	EntityKeywordMode string
	// HighLevelKeywordsToSparse 가 true 이면 EntityKeywordsLLM 이 뽑은 고수준
	// (주제) 키워드를 희소 레인(fts·bigm) 키워드에 덧붙인다. SparseQuery 가
	// raw 이면 덧붙일 키워드 경로가 없으므로 Normalized 가 false 로 되돌린다.
	// vec·summvec 레인은 건드리지 않는다.
	HighLevelKeywordsToSparse bool
	// GraphWeight 는 그래프 1-hop 레인(entity_relations)의 RRF 가중치다.
	// 0(기본)이면 레인 자체가 SQL 에 들어가지 않는다. 시드 엔티티는
	// EntityKeywordMode 의 저수준 키워드로 찾으므로, 모드가 꺼져 있거나
	// 키워드가 없는 질의에서는 이 값이 있어도 레인이 생기지 않는다.
	GraphWeight float64

	// GraphHubDamping 이 true 이면 그래프 레인이 관계 근거 문서를
	// SUM(confidence * seed_weight) 로 순위 매긴다. seed_weight =
	// 1/ln(e + 시드 엔티티의 document_entities 행 수)(HippoRAG 의 노드 특이성).
	// 사용자 본인·가족처럼 거의 모든 문서에 나오는 허브 엔티티가 레인을
	// 채우는 것을 누른다. GraphWeight 가 0 이면 레인이 없으므로 Normalized 가
	// false 로 되돌린다.
	GraphHubDamping bool
	// GraphExpandBoost 는 결과 시드 그래프 확장(Graphiti edge_search·Hindsight)의
	// 승수 강도다. 0(기본)이면 끈다. 융합 상위 문서의 엔티티 ∪ 키워드 시드
	// 엔티티와 관계로 이어진 기존 후보만 score *= 1 + boost*s (s∈[0,1]) 로
	// 올린다 — 새 문서를 더하지 않는 재정렬이다. 1 을 넘으면 1 로 자른다.
	GraphExpandBoost float64
	// RRFMissingRank 는 RRFMissingRankZero(기본) 또는 RRFMissingRankCutoff.
	RRFMissingRank string
}

// IsZero 는 노브가 하나도 설정되지 않았는지 — 즉 "현행 동작" 인지 — 알린다.
// SearchQuery.Weights 가 제로값일 때 서비스 기본값으로 대체되는 것과 같은
// 판정에 쓴다.
func (t SearchTuning) IsZero() bool { return t == SearchTuning{} }

// Normalized 는 빈 값·잘못된 값을 기본값으로 채운 복사본을 돌려준다.
// 검색 경로는 반드시 이 결과만 읽는다: 잘못된 값 때문에 검색이 실패하는
// 것보다 현행 동작으로 되돌아가는 쪽이 항상 낫다.
func (t SearchTuning) Normalized() SearchTuning {
	if t.RerankOverfetch < 0 {
		t.RerankOverfetch = 0
	}
	if t.MergeMode != MergeSymmetric {
		t.MergeMode = MergeAsymmetric
	}
	if t.RerankBlend != RerankBlendRRF {
		t.RerankBlend = RerankBlendReplace
	}
	if t.RerankBlendWeight <= 0 || isBadFloat(t.RerankBlendWeight) {
		t.RerankBlendWeight = DefaultRerankBlendWeight
	}
	if t.RerankInput != RerankInputBestChunk {
		t.RerankInput = RerankInputHead
	}
	if t.RecencyHalfLifeDays < 0 || isBadFloat(t.RecencyHalfLifeDays) {
		t.RecencyHalfLifeDays = 0
	}
	if t.RecencyAlpha <= 0 || isBadFloat(t.RecencyAlpha) {
		t.RecencyAlpha = DefaultRecencyAlpha
	}
	if t.RecencyAlpha > 1 {
		t.RecencyAlpha = 1
	}
	if t.ChunkSparse != ChunkSparseFuse && t.ChunkSparse != ChunkSparseFuseCtx {
		t.ChunkSparse = ChunkSparseFallback
	}
	if t.ChunkSparse == ChunkSparseFuseCtx &&
		t.ChunkSparseCtxVersion != ChunkSparseCtxV1TP && t.ChunkSparseCtxVersion != ChunkSparseCtxV1Full {
		// 버전이 없거나 알 수 없으면 fuse_ctx 를 켰다고 믿지만 매 요청이
		// 아무 문맥도 찾지 못해 조용히 새는 상태가 된다 — 그보다는 검색이
		// 멈추지 않는 현행 동작(fallback)으로 되돌린다.
		t.ChunkSparse = ChunkSparseFallback
	}
	if t.ChunkSparse != ChunkSparseFuseCtx {
		t.ChunkSparseCtxVersion = ""
	}
	if t.SparseQuery != SparseQueryChunk && t.SparseQuery != SparseQueryChunkDoc {
		t.SparseQuery = SparseQueryRaw
	}
	if t.EntityKeywordMode != EntityKeywordsSparse && t.EntityKeywordMode != EntityKeywordsLLM {
		t.EntityKeywordMode = EntityKeywordsOff
	}
	// 고수준 키워드는 LLM 모드에서만 생기고, 덧붙일 희소 키워드 경로(raw 가
	// 아닌 SparseQuery)가 있어야 쓰인다. 효과가 없는 조합을 켠 채로 두면
	// 실행 프로필(config_hash)만 갈라진다.
	if t.EntityKeywordMode != EntityKeywordsLLM || t.SparseQuery == SparseQueryRaw {
		t.HighLevelKeywordsToSparse = false
	}
	if t.GraphWeight < 0 || isBadFloat(t.GraphWeight) {
		t.GraphWeight = 0
	}
	// 허브 감쇠는 그래프 레인의 순위 식이라, 레인이 없으면 효과가 없다.
	if t.GraphWeight == 0 {
		t.GraphHubDamping = false
	}
	if t.GraphExpandBoost < 0 || isBadFloat(t.GraphExpandBoost) {
		t.GraphExpandBoost = 0
	}
	if t.GraphExpandBoost > 1 {
		t.GraphExpandBoost = 1
	}
	if t.RRFMissingRank != RRFMissingRankCutoff {
		t.RRFMissingRank = RRFMissingRankZero
	}
	return t
}

// EnvSearchTuning 은 환경변수에서 노브를 읽는다. 아무것도 설정하지 않은
// 배포는 제로값을 받으므로 현행 동작 그대로다.
//
// model 패키지가 직접 환경변수를 읽는 것은 이 리포의 기존 방식이다
// (LowRetentionPenalty / SummaryVecCoverageThreshold 참고). 덕분에
// internal/search.NewService 가 config 를 몰라도 운영 배포에서 노브가 켜지고,
// cmd/server·cmd/mcp 배선을 바꾸지 않아도 된다.
//
// 잘못된 값은 경고를 남기고 무시한다 — 오타 하나로 검색이 멈추면 안 된다.
func EnvSearchTuning() SearchTuning {
	t := SearchTuning{
		EntityQueryContainsName: envTuningChoice("SEARCH_ENTITY_QUERY_CONTAINS_NAME", "false", "true") == "true",
		RerankCallContext:       envTuningChoice("SEARCH_RERANK_CALL_CONTEXT", "false", "true") == "true",
		RerankOverfetch:         envTuningInt("SEARCH_RERANK_OVERFETCH"),
		MergeMode:               envTuningChoice("SEARCH_MERGE_MODE", MergeAsymmetric, MergeSymmetric),
		RerankBlend:             envTuningChoice("SEARCH_RERANK_BLEND", RerankBlendReplace, RerankBlendRRF),
		RerankBlendWeight:       envTuningFloat("SEARCH_RERANK_BLEND_WEIGHT"),
		RerankInput:             envTuningChoice("SEARCH_RERANK_INPUT", RerankInputHead, RerankInputBestChunk),
		RecencyHalfLifeDays:     envTuningFloat("SEARCH_RECENCY_HALFLIFE_DAYS"),
		RecencyAlpha:            envTuningFloat("SEARCH_RECENCY_ALPHA"),
		ChunkSparse:             envTuningChoice("SEARCH_CHUNK_SPARSE", ChunkSparseFallback, ChunkSparseFuse, ChunkSparseFuseCtx),
		ChunkSparseCtxVersion:   envChunkSparseCtxVersion(),
		SparseQuery:             envTuningChoice("SEARCH_SPARSE_QUERY", SparseQueryRaw, SparseQueryChunk, SparseQueryChunkDoc),

		EntityKeywordMode:         envTuningChoice("SEARCH_ENTITY_KEYWORDS", EntityKeywordsOff, EntityKeywordsSparse, EntityKeywordsLLM),
		HighLevelKeywordsToSparse: envTuningChoice("SEARCH_HIGH_LEVEL_KEYWORDS_TO_SPARSE", "false", "true") == "true",
		GraphWeight:               envTuningFloat("SEARCH_GRAPH_WEIGHT"),

		GraphHubDamping:  envTuningChoice("SEARCH_GRAPH_HUB_DAMPING", "false", "true") == "true",
		GraphExpandBoost: envTuningFloat("SEARCH_GRAPH_EXPAND_BOOST"),
		RRFMissingRank:   envTuningChoice("SEARCH_RRF_MISSING_RANK", RRFMissingRankZero, RRFMissingRankCutoff),
	}
	n := t.Normalized()
	if t.HighLevelKeywordsToSparse && !n.HighLevelKeywordsToSparse {
		slog.Warn("search tuning: SEARCH_HIGH_LEVEL_KEYWORDS_TO_SPARSE has no effect, ignoring",
			"requires", "SEARCH_ENTITY_KEYWORDS=llm and SEARCH_SPARSE_QUERY=chunk|chunk_doc")
	}
	if n.GraphWeight > 0 && n.EntityKeywordMode == EntityKeywordsOff {
		slog.Warn("search tuning: SEARCH_GRAPH_WEIGHT needs SEARCH_ENTITY_KEYWORDS, graph lane will stay empty")
	}
	if t.GraphHubDamping && !n.GraphHubDamping {
		slog.Warn("search tuning: SEARCH_GRAPH_HUB_DAMPING has no effect, ignoring",
			"requires", "SEARCH_GRAPH_WEIGHT>0")
	}
	if t.GraphExpandBoost > 1 {
		slog.Warn("search tuning: SEARCH_GRAPH_EXPAND_BOOST above 1, clamping",
			"key", "SEARCH_GRAPH_EXPAND_BOOST", "max", 1)
	}
	return n
}

func envTuningInt(key string) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		slog.Warn("search tuning: invalid integer, ignoring", "key", key, "value", raw)
		return 0
	}
	return n
}

func envTuningFloat(key string) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f < 0 || isBadFloat(f) {
		slog.Warn("search tuning: invalid number, ignoring", "key", key, "value", raw)
		return 0
	}
	return f
}

// envChunkSparseCtxVersion 은 SEARCH_CHUNK_SPARSE_CTX 를 읽는다. envTuningChoice
// 를 쓰지 않는 이유: 이 값은 "설정 안 함"(빈 문자열)이 그 자체로 유효한
// 기본값이고, fuse_ctx 가 아닌 모드에서는 있어도 무시되므로 강제할 기본값이
// 없다 — SearchTuning.Normalized 가 fuse_ctx 조합에서만 비어 있는지 검사한다.
func envChunkSparseCtxVersion() string {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("SEARCH_CHUNK_SPARSE_CTX")))
	switch raw {
	case "", ChunkSparseCtxV1TP, ChunkSparseCtxV1Full:
		return raw
	default:
		slog.Warn("search tuning: unknown value, ignoring",
			"key", "SEARCH_CHUNK_SPARSE_CTX", "value", raw)
		return ""
	}
}

// envTuningChoice 는 허용된 값 목록 중 하나만 받는다. 목록의 첫 값이 기본값이며,
// 알 수 없는 값은 경고 후 기본값으로 되돌린다.
func envTuningChoice(key string, allowed ...string) string {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if raw == "" {
		return allowed[0]
	}
	for _, a := range allowed {
		if raw == a {
			return raw
		}
	}
	slog.Warn("search tuning: unknown value, using default",
		"key", key, "value", raw, "default", allowed[0], "allowed", allowed)
	return allowed[0]
}
