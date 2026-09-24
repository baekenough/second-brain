package search

import (
	"context"
	"log/slog"
	"math"
	"time"
	"unicode/utf8"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
	"github.com/google/uuid"
)

// rrfK 는 이 패키지가 쓰는 RRF 상수다. 문서 스토어의 융합(buildRRFScoreExpr)과
// mergeRRF 가 이미 60 을 쓰고 있어, 리랭크 합산도 같은 값을 쓴다 — 두 순위를
// 합산하려면 두 항의 스케일이 같아야 한다.
const rrfK = 60.0

// ChunkLister 는 문서 하나의 청크를 순서대로 돌려준다. *store.ChunkStore 가
// 만족하며, RerankInputBestChunk 에서만 쓴다.
//
// 별도 인터페이스로 둔 이유: ChunkSearcher(코퍼스 전역 검색)와 목적이 다르고,
// 이걸 만족하지 않는 테스트 더블은 조용히 head 입력으로 되돌아가야 하기
// 때문이다. 인터페이스 단언 실패가 검색 실패가 되어서는 안 된다.
type ChunkLister interface {
	ListByDocument(ctx context.Context, documentID uuid.UUID) ([]store.Chunk, error)
}

// ChunkBatchLister 는 여러 문서의 청크를 한 번에 돌려주는 선택 인터페이스다.
// *store.ChunkStore 가 만족한다. 청크가 없는 문서는 맵에 키가 없어도 되고,
// 문서별 슬라이스는 ListByDocument 와 같은 chunk_index 오름차순이어야 한다.
//
// ChunkLister 에 메서드를 더하지 않고 따로 둔 이유: 기존 더블(askeval 가짜
// 코퍼스 등)을 건드리지 않기 위해서다. 이걸 만족하지 않으면 buildRerankDocs 는
// 예전처럼 문서마다 ListByDocument 를 부른다 — 결과 텍스트는 두 경로가 같다.
type ChunkBatchLister interface {
	ListByDocuments(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]store.Chunk, error)
}

// 운영 배선이 조용히 N회 경로로 떨어지지 않게 컴파일 시점에 고정한다.
var (
	_ ChunkLister      = (*store.ChunkStore)(nil)
	_ ChunkBatchLister = (*store.ChunkStore)(nil)
)

// resolveTuning 은 이 요청에 적용할 노브를 정한다. 요청이 아무것도 지정하지
// 않았으면 서비스 기본값(= 환경변수)을 쓴다. Weights 필드와 같은 규약이다.
func (s *Service) resolveTuning(q model.SearchQuery) model.SearchTuning {
	if q.Tuning.IsZero() {
		return s.tuning.Normalized()
	}
	return q.Tuning.Normalized()
}

// rerankPoolLimit 은 리랭크가 켜진 요청의 후보 풀 하한을 적용한다.
//
// 실측 근거: limit=10 일 때 현행 풀은 20건뿐이라, 사용자 판정 정답 24건 중
// 7건이 리랭커에 도달조차 하지 못했다. 리랭커가 순위를 못 고친 게 아니라
// 고칠 대상을 못 본 것이다. 하한을 키우면 "리랭커가 나쁘다" 와 "리랭커가
// 못 봤다" 가 비로소 구분된다.
//
// 상한(overfetchLimitCap)은 그대로 적용한다. 노브는 실험용이고, 실험용
// 설정이 레인 질의를 무한정 키울 수 있어서는 안 된다.
func rerankPoolLimit(base, overfetch int) int {
	if overfetch <= 0 {
		return base
	}
	if overfetch > overfetchLimitCap {
		overfetch = overfetchLimitCap
	}
	if overfetch > base {
		return overfetch
	}
	return base
}

// blendRerankRRF 는 융합 순위와 리랭커 순위를 RRF 로 합산해 다시 정렬한다.
//
//	score = 1/(k + fused_rank) + weight * 1/(k + rerank_rank)
//
// 대체(replace)와의 차이는 "리랭커가 틀렸을 때 얼마나 망가지는가" 다.
// 대체에서는 융합이 1위로 올린 문서를 리랭커가 20위로 내리면 그대로 20위가
// 되지만, 합산에서는 1/(60+1) 항이 남아 상위권을 지킨다. 실측에서 리랭커가
// 정답 3건을 상위 10위 밖으로 밀어냈던 것이 이 항이 없었기 때문이다.
//
// fused 에 없던 문서(리랭커가 돌려준 목록에만 있는 경우)는 리랭커 항만
// 갖는다. 실제로는 applyRerank 가 입력 전체를 돌려주므로 발생하지 않지만,
// 구현이 바뀌어도 문서가 조용히 사라지지 않게 해 둔다.
func blendRerankRRF(fused, reranked []*model.SearchResult, weight float64) []*model.SearchResult {
	if len(reranked) == 0 {
		return fused
	}
	fusedRank := make(map[uuid.UUID]int, len(fused))
	for i, r := range fused {
		if _, seen := fusedRank[r.ID]; !seen {
			fusedRank[r.ID] = i + 1
		}
	}

	out := make([]*model.SearchResult, 0, len(reranked))
	for i, r := range reranked {
		cp := *r // 얕은 복사 — 호출자의 슬라이스를 건드리지 않는다
		score := weight / (rrfK + float64(i+1))
		if fr, ok := fusedRank[r.ID]; ok {
			score += 1.0 / (rrfK + float64(fr))
		}
		cp.Score = score
		out = append(out, &cp)
	}
	sortByScore(out)
	return out
}

// applyRecencyDecay 는 융합 점수에 최신성 승수를 곱한다.
//
//	multiplier = (1-alpha) + alpha * exp(-ln2 * age_days / halflife)
//
// 세 가지를 의도적으로 하지 않는다:
//
//   - 시간창(OccurredFrom/OccurredTo)이 있는 질의에는 적용하지 않는다.
//     기간을 이미 지정한 질의에 최신성을 또 얹으면, 사용자가 고른 기간
//     안에서 순서가 조용히 뒤집힌다.
//   - occurred_at 이 NULL 인 문서는 건드리지 않는다. "사건 시각을 모른다" 와
//     "오래됐다" 는 다른 사실이고, 수집 시각(collected_at)으로 대용하면
//     코퍼스의 상당 부분이 이유 없이 최신 취급을 받는다.
//   - 미래 문서(age < 0)도 감쇠하지 않는다. 다가오는 일정을 "아직 일어나지
//     않았으니 관련 없다" 고 밀어내는 것은 이 노브의 목적이 아니다.
//
// Sort="recent" 요청과는 무관하다 — 그쪽은 정렬 규칙이고 이건 점수다.
// 그래서 SortsByRecency 인 요청에서는 재정렬하지 않고 점수만 조정한다.
func applyRecencyDecay(q model.SearchQuery, results []*model.SearchResult, tune model.SearchTuning, now time.Time) []*model.SearchResult {
	if tune.RecencyHalfLifeDays <= 0 || len(results) == 0 {
		return results
	}
	if q.OccurredFrom != nil || q.OccurredTo != nil {
		return results
	}

	changed := false
	out := make([]*model.SearchResult, len(results))
	for i, r := range results {
		out[i] = r
		if r.OccurredAt == nil {
			continue
		}
		ageDays := now.Sub(*r.OccurredAt).Hours() / 24
		if ageDays <= 0 {
			continue
		}
		decay := math.Exp(-math.Ln2 * ageDays / tune.RecencyHalfLifeDays)
		multiplier := (1 - tune.RecencyAlpha) + tune.RecencyAlpha*decay
		cp := *r // 얕은 복사 — 레인 결과를 변형하지 않는다
		cp.Score *= multiplier
		out[i] = &cp
		changed = true
	}
	if changed && !q.SortsByRecency() {
		sortByScore(out)
	}
	return out
}

// sourceLabelKO 는 리랭커 입력 머리글에 쓰는 소스 한글 라벨이다.
// 리랭커가 받는 것은 잘린 본문뿐이라 "이게 통화인지 메일인지" 를 알 방법이
// 없다. 한 줄짜리 머리글이 그 맥락을 준다. 모르는 소스는 원래 값을 그대로 쓴다.
var sourceLabelKO = map[model.SourceType]string{
	model.SourceCall:           "통화",
	model.SourceCallLog:        "통화기록",
	model.SourceCallTranscript: "통화전사",
	model.SourceSMS:            "문자",
	model.SourceGmail:          "메일",
	model.SourceCalendar:       "일정",
	model.SourceNote:           "노트",
	model.SourceAgentNote:      "에이전트노트",
	model.SourceInsight:        "인사이트",
	model.SourceUpload:         "업로드",
	model.SourceFilesystem:     "파일",
	model.SourceSlack:          "슬랙",
	model.SourceDiscord:        "디스코드",
	model.SourceTelegram:       "텔레그램",
	model.SourceNotion:         "노션",
	model.SourceGitHub:         "깃허브",
	model.SourceGDrive:         "드라이브",
	model.SourceLLMMemory:      "대화기록",
	model.SourceSecretary:      "비서",
}

func sourceLabel(st model.SourceType) string {
	if label, ok := sourceLabelKO[st]; ok {
		return label
	}
	if st == "" {
		return "문서"
	}
	return string(st)
}

// rerankHeader 는 "[통화 · 2026-09-01 · 제목]" 형태의 머리글 한 줄이다.
// 날짜는 일 단위까지만 넣는다 — 리랭커에 보내는 텍스트에 시·분까지 실으면
// 특정 통화를 지목하는 단서가 하나 더 늘어날 뿐 순위에는 기여하지 않는다.
func rerankHeader(r *model.SearchResult) string {
	date := ""
	if r.OccurredAt != nil {
		date = r.OccurredAt.Format(time.DateOnly)
	}
	head := "[" + sourceLabel(r.SourceType)
	if date != "" {
		head += " · " + date
	}
	if r.Title != "" {
		head += " · " + r.Title
	}
	return head + "]"
}

// isChunkResult 는 이 결과가 청크 레인에서 온 것인지 — 즉 Content 가 이미
// "질의와 가장 잘 맞는 청크" 인지 — 알린다. 이 경우 DB 를 다시 읽을 필요가 없다.
func isChunkResult(r *model.SearchResult) bool {
	return r.MatchType == "chunk-vector" || r.MatchType == "chunk-fts"
}

// maxRerankDocRunes 는 리랭커 한 문서당 보내는 rune 수 상한이다. 노브를
// 켜도 이 값은 바뀌지 않는다 — best_chunk 가 바꾸는 것은 "같은 예산으로
// 무엇을 보내는가" 이지 예산 자체가 아니다.
const maxRerankDocRunes = 1000

// buildRerankDocs 는 리랭커에 보낼 문서 텍스트를 만든다.
//
//   - model.RerankInputHead(기본): 제목 + 본문 앞부분 절단. 현행 동작.
//   - model.RerankInputBestChunk: "[소스 · 날짜 · 제목]" 머리글 한 줄 +
//     질의와 가장 잘 맞는 청크 본문.
//
// head 가 약한 이유(실측): 통화 전사와 긴 메일은 근거가 본문 중간에 있는데
// 앞 1000 rune 만 보내면 리랭커가 보는 것은 인사말과 서명뿐이다. 판정 정답
// 통화 5건이 리랭크 후 5·9·10위와 탈락 2건이 된 실행에서, 리랭커가 받은
// 텍스트에는 질의어가 아예 없었다.
//
// DB 왕복: 청크 스토어가 ChunkBatchLister 를 만족하면 요청당 최대 1회(#263),
// 아니면 문서당 최대 1회다. 단건 경로에서 조회가 실패하면 그 문서만, 배치
// 경로에서 실패하면 그 한 번의 조회에 걸린 문서 전부가 head 로 되돌아간다 —
// 어느 쪽이든 "청크를 못 읽은 문서는 head" 라는 같은 규칙이다.
func (s *Service) buildRerankDocs(ctx context.Context, query string,
	results []*model.SearchResult, tune model.SearchTuning) []string {
	docs := make([]string, len(results))
	if tune.RerankInput != model.RerankInputBestChunk {
		for i, r := range results {
			docs[i] = truncateRunes(r.Title+"\n"+r.Content, maxRerankDocRunes)
		}
		return docs
	}

	lister := s.chunkLister()
	batch, batched := prefetchChunks(ctx, s.chunkBatchLister(), results)
	for i, r := range results {
		var body string
		if batched {
			body = bestChunkFromBatch(query, r, batch)
		} else {
			body = bestChunkText(ctx, lister, query, r)
		}
		if body == "" {
			body = r.Content // head 폴백: 청크를 못 읽었거나 없는 문서
		}
		docs[i] = truncateRunes(rerankHeader(r)+"\n"+body, maxRerankDocRunes)
	}
	return docs
}

// bestChunkText 는 문서에서 질의와 가장 잘 맞는 청크 본문을 고른다.
// DB 왕복은 문서당 최대 1회이며, 실패하거나 청크가 없으면 빈 문자열을 돌려
// 호출자가 head 로 되돌아가게 한다.
func bestChunkText(ctx context.Context, lister ChunkLister, query string, r *model.SearchResult) string {
	if isChunkResult(r) {
		return r.Content
	}
	if lister == nil {
		return ""
	}
	chunks, err := lister.ListByDocument(ctx, r.ID)
	if err != nil {
		// 비치명적: 이 문서 하나만 head 입력으로 되돌아간다. 본문 길이만
		// 남기고 내용은 절대 로그에 싣지 않는다.
		slog.Warn("search: best-chunk lookup failed, falling back to head input",
			"error", err, "document_id", r.ID)
		return ""
	}
	return pickBestChunk(query, chunks)
}

// prefetchChunks 는 청크 레인이 아닌 결과들의 청크를 한 번에 읽는다.
//
// 두 번째 반환값은 "배치 경로를 탔는가" 다. false 이면 호출자는 기존 단건
// 경로(bestChunkText)로 간다 — 배치 조회기가 없을 때만 그렇다. 배치 조회가
// 실패했을 때는 true 와 nil 맵을 돌려, 단건으로 N회 재시도하지 않고 대상 문서
// 전부가 head 로 되돌아가게 한다(단건 경로의 실패 의미와 같다: 못 읽은 문서는
// head). 이미 실패한 DB 에 같은 요청 안에서 N번 더 두드릴 이유가 없다.
//
// 조회할 문서가 하나도 없으면(전부 청크 레인 결과) DB 를 건드리지 않는다.
func prefetchChunks(ctx context.Context, bl ChunkBatchLister,
	results []*model.SearchResult) (map[uuid.UUID][]store.Chunk, bool) {
	if bl == nil {
		return nil, false
	}
	ids := make([]uuid.UUID, 0, len(results))
	seen := make(map[uuid.UUID]struct{}, len(results))
	for _, r := range results {
		if isChunkResult(r) {
			continue
		}
		if _, dup := seen[r.ID]; dup {
			continue
		}
		seen[r.ID] = struct{}{}
		ids = append(ids, r.ID)
	}
	if len(ids) == 0 {
		return nil, true
	}
	byDoc, err := bl.ListByDocuments(ctx, ids)
	if err != nil {
		// 비치명적: 이번 요청의 대상 문서 전부가 head 입력으로 되돌아간다.
		// 문서 수만 남기고 내용은 절대 로그에 싣지 않는다.
		slog.Warn("search: batched best-chunk lookup failed, falling back to head input",
			"error", err, "documents", len(ids))
		return nil, true
	}
	return byDoc, true
}

// bestChunkFromBatch 는 prefetchChunks 가 읽어 둔 청크로 bestChunkText 와 같은
// 답을 낸다. 청크 레인 결과는 그대로, 맵에 없는 문서(청크 없음·조회 실패)는
// 빈 문자열이다.
func bestChunkFromBatch(query string, r *model.SearchResult, byDoc map[uuid.UUID][]store.Chunk) string {
	if isChunkResult(r) {
		return r.Content
	}
	return pickBestChunk(query, byDoc[r.ID])
}

// pickBestChunk 는 chunk_index 오름차순으로 정렬된 청크 중 질의와 가장 잘
// 맞는 본문을 고른다. 청크가 없으면 빈 문자열이다. 단건·배치 두 경로가 이
// 함수 하나를 공유하므로 선택 규칙은 여기서만 정의된다.
func pickBestChunk(query string, chunks []store.Chunk) string {
	if len(chunks) == 0 {
		return ""
	}

	// 청크 임베딩은 이 경로에서 읽을 수 없으므로(ListByDocument 가 벡터를
	// 돌려주지 않는다) 질의와의 문자 바이그램 겹침으로 고른다. 한국어에서는
	// 형태소 경계를 몰라도 바이그램 겹침이 꽤 잘 듣고, 이 리포의 FTS 레인이
	// 쓰는 pg_bigm 과 같은 단위다. 동점이면 chunk_index 가 작은 쪽 —
	// ListByDocument/ListByDocuments 가 인덱스 오름차순을 보장하므로 결정론적이다.
	qgrams := bigrams(query)
	if len(qgrams) == 0 {
		return chunks[0].Content
	}
	best, bestScore := chunks[0].Content, -1
	for _, c := range chunks {
		score := 0
		cg := bigrams(c.Content)
		for g := range qgrams {
			if cg[g] {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = c.Content, score
		}
	}
	return best
}

// bigrams 는 문자 2-gram 집합을 만든다. 1글자 질의는 그 글자 자체를 원소로 쓴다.
func bigrams(s string) map[string]bool {
	runes := []rune(s)
	out := make(map[string]bool, len(runes))
	switch {
	case len(runes) == 0:
		return out
	case len(runes) == 1:
		out[string(runes)] = true
	default:
		for i := 0; i+1 < len(runes); i++ {
			out[string(runes[i:i+2])] = true
		}
	}
	return out
}

// truncateRunes 는 rune 수 기준으로 자른다. 바이트로 자르면 한글 한 글자가
// 쪼개져 깨진 문자가 리랭커에 간다.
func truncateRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}
