package search

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/sparseq"
	"github.com/baekenough/second-brain/internal/store"
)

// EntityKeywordPromptVersion 은 LLM 키워드 추출 프롬프트의 판이다. 프롬프트를
// 바꾸면 같은 노브 값이라도 추출 결과가 달라지므로 반드시 올린다. 평가 실행
// 프로필(cmd/eval 의 entity_keyword_prompt_version)에 실려, 프롬프트가 다른
// 실행이 같은 baseline 계열로 섞이지 않게 한다.
const EntityKeywordPromptVersion = "v1"

// keywordLLMTimeout 은 키워드 추출 LLM 호출 한 번의 상한이다. HyDE(Expand)는
// 별도 상한 없이 LLM 클라이언트의 HTTP 타임아웃(기본 60초)에 기대지만, 이
// 호출은 검색 임베딩보다 앞에서 직렬로 돌기 때문에 느린 LLM 이 검색 지연으로
// 그대로 번지지 않게 더 짧게 끊는다. 초과하면 sparse 결과로 되돌아간다.
// 테스트가 타임아웃 경로를 짧게 검증할 수 있도록 var 로 둔다.
var keywordLLMTimeout = 8 * time.Second

// maxKeywordResponseBytes 는 파싱을 시도할 LLM 응답 크기 상한이다. 정상 응답은
// 수백 바이트이고, 이보다 큰 응답은 형식을 어긴 것이라 파싱하지 않는다.
const maxKeywordResponseBytes = 16 << 10

const entityKeywordSystemPrompt = `당신은 개인 지식 검색 시스템의 질의 분석기입니다. 사용자의 질문에서 두 종류의 키워드를 뽑아 JSON 객체 하나로만 답하세요.

- low_level: 질문에 직접 나오는 구체적인 이름 — 사람, 조직, 장소, 제품, 프로젝트 같은 고유명사. 질문에 적힌 표기 그대로 쓰고, 한국어는 조사를 뗀 형태("김철수가" -> "김철수")로 씁니다.
- high_level: 질문이 다루는 주제·개념·활동 (예: "회의 일정", "계약 협상", "여행 계획").

규칙:
- 각 목록은 최대 8개, 항목 하나는 40자 이하.
- 질문에 없는 이름을 지어내지 마세요. 해당하는 것이 없으면 빈 목록을 쓰세요.
- JSON 외의 설명, 마크다운, 코드펜스를 쓰지 마세요.

출력 형식:
{"low_level": ["..."], "high_level": ["..."]}`

// queryKeywords 는 질문에서 뽑은 이중 키워드다. 둘 다 질문에서 파생된 개인
// 데이터이므로 로그·에러·메트릭 라벨에 내용을 싣지 않는다 — 개수만 남긴다
// (model.QueryPlan.Reason 과 llm.TruncatedError 가 문서화한 규칙).
type queryKeywords struct {
	// Low 는 엔티티·그래프 레인의 시드 키워드다(store.NormalizeEntityKeywords 를
	// 거친 형태).
	Low []string
	// High 는 희소 레인에 덧붙일 주제 키워드다. EntityKeywordsLLM 에서만 생긴다.
	High []string
}

// 로그에 남기는 추출 경로와 실패 사유. 고정 문자열만 쓴다 — 에러 메시지나
// 응답 조각을 그대로 싣지 않기 위해서다.
const (
	kwSourceSparse      = "sparse"
	kwSourceLLM         = "llm"
	kwSourceLLMFallback = "llm_fallback_sparse"

	kwReasonDisabled   = "disabled"
	kwReasonTimeout    = "timeout"
	kwReasonTruncated  = "truncated"
	kwReasonCallFailed = "call_failed"
	kwReasonBadJSON    = "bad_json"
	kwReasonEmpty      = "empty"
)

// queryKeywordsFor 는 EntityKeywordMode 에 따라 질문에서 키워드를 뽑는다.
// 어떤 경우에도 에러를 돌려주지 않는다: LLM 이 없거나 느리거나 엉뚱한 응답을
// 줘도 sparse 결과로 되돌아가며, 검색 자체는 계속된다.
func (s *Service) queryKeywordsFor(ctx context.Context, query string, tune model.SearchTuning) queryKeywords {
	switch tune.EntityKeywordMode {
	case model.EntityKeywordsSparse:
		kw := sparseKeywords(query)
		logKeywords(ctx, tune.EntityKeywordMode, kwSourceSparse, "", kw)
		return kw
	case model.EntityKeywordsLLM:
		kw, reason := llmKeywords(ctx, s.llmClient, query)
		if len(kw.Low) == 0 {
			// 고유명사가 하나도 안 나오면 엔티티 레인에 시드가 없다. sparse 로
			// 대신 채우되, LLM 이 준 주제 키워드(High)는 버리지 않는다.
			kw.Low = sparseKeywords(query).Low
			if reason == "" {
				reason = kwReasonEmpty
			}
		}
		source := kwSourceLLM
		if reason != "" {
			source = kwSourceLLMFallback
		}
		logKeywords(ctx, tune.EntityKeywordMode, source, reason, kw)
		return kw
	default:
		return queryKeywords{}
	}
}

// sparseKeywords 는 internal/sparseq 로 저수준 키워드를 뽑는다(LLM 호출 없음).
// LIKE 용 부분 문자열 목록을 쓰는 이유: TS 렉심은 그 목록의 소문자 부분집합이고,
// 이메일·전화번호처럼 구분자가 든 정확 토큰은 Like 쪽에만 한 덩어리로 남는다.
func sparseKeywords(query string) queryKeywords {
	return queryKeywords{Low: store.NormalizeEntityKeywords(sparseq.Extract(query).Like)}
}

// keywordResponse 는 LLM 이 돌려주기로 한 JSON 이다.
type keywordResponse struct {
	Low  []string `json:"low_level"`
	High []string `json:"high_level"`
}

// llmKeywords 는 LLM 한 번으로 이중 키워드를 뽑는다. 실패 사유(고정 문자열)를
// 함께 돌려주며, 성공이면 사유가 빈 문자열이다. 실패 시 키워드는 비어 있다.
func llmKeywords(ctx context.Context, client llm.Completer, query string) (queryKeywords, string) {
	if client == nil || !client.Enabled() {
		return queryKeywords{}, kwReasonDisabled
	}

	callCtx, cancel := context.WithTimeout(ctx, keywordLLMTimeout)
	defer cancel()
	raw, err := client.CompleteWithMessages(callCtx, entityKeywordSystemPrompt,
		[]llm.Message{{Role: "user", Content: query}})
	if err != nil {
		// err 의 문자열은 쓰지 않는다 — 클라이언트 구현에 따라 요청·응답 조각이
		// 들어 있을 수 있다(질문은 개인 데이터다). 종류만 구분해 남긴다.
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return queryKeywords{}, kwReasonTimeout
		case errors.Is(err, llm.ErrTruncated):
			return queryKeywords{}, kwReasonTruncated
		default:
			return queryKeywords{}, kwReasonCallFailed
		}
	}

	kw, ok := parseKeywordResponse(raw)
	if !ok {
		return queryKeywords{}, kwReasonBadJSON
	}
	if len(kw.Low) == 0 && len(kw.High) == 0 {
		return queryKeywords{}, kwReasonEmpty
	}
	return kw, ""
}

// parseKeywordResponse 는 LLM 응답에서 키워드 JSON 을 꺼내 정규화한다. 모델이
// 지시를 어기고 ```json 펜스나 앞뒤 설명을 붙이는 경우를 견디기 위해 첫 '{'
// 부터 마지막 '}' 까지만 잘라 파싱한다. 형식이 틀리면 ok=false 다.
func parseKeywordResponse(raw string) (queryKeywords, bool) {
	if len(raw) > maxKeywordResponseBytes {
		return queryKeywords{}, false
	}
	start := strings.IndexByte(raw, '{')
	end := strings.LastIndexByte(raw, '}')
	if start < 0 || end <= start {
		return queryKeywords{}, false
	}
	var resp keywordResponse
	if err := json.Unmarshal([]byte(raw[start:end+1]), &resp); err != nil {
		return queryKeywords{}, false
	}
	// 개수·길이·중복·UTF-8 방어는 저장소와 같은 규칙을 쓴다. 상한(8개, 40자)은
	// 항목 수를 먼저 자르지 않고 정규화가 유효 항목만 센 뒤 자른다 — 앞쪽에
	// 쓸모없는 항목(너무 짧거나 긴 것)이 있어도 뒤의 유효 항목을 잃지 않는다.
	return queryKeywords{
		Low:  store.NormalizeEntityKeywords(resp.Low),
		High: store.NormalizeEntityKeywords(resp.High),
	}, true
}

// logKeywords 는 추출 결과를 개수로만 남긴다. 키워드 내용·질문·LLM 응답은
// 절대 싣지 않는다.
func logKeywords(ctx context.Context, mode, source, reason string, kw queryKeywords) {
	attrs := []any{
		"mode", mode,
		"source", source,
		"low_count", len(kw.Low),
		"high_count", len(kw.High),
	}
	if reason != "" {
		attrs = append(attrs, "fallback_reason", reason)
	}
	if source == kwSourceLLMFallback && reason != kwReasonDisabled && reason != kwReasonEmpty {
		slog.WarnContext(ctx, "search: entity keyword llm failed, using sparse keywords", attrs...)
		return
	}
	slog.DebugContext(ctx, "search: entity keywords extracted", attrs...)
}

// sparseTermsWithHighLevel 은 sparseTermsFor 의 결과에 고수준 키워드를 덧붙인다
// (HighLevelKeywordsToSparse). 덧붙일 것이 없거나 노브가 꺼져 있으면 sparseTermsFor
// 와 똑같은 값을 돌려준다.
//
// 고수준 키워드는 "회의 일정" 같은 구절이라 sparseq.Extract 로 다시 풀어 같은
// 어휘 규칙(조사·불용어 제거, 렉심 검증)을 거친다 — tsquery 에 검증 안 된 문자열이
// 들어가는 길을 만들지 않기 위해서다. 질문에서 직접 뽑은 키워드가 먼저 자리를
// 차지하고, 고수준 키워드는 sparseq.MaxTerms 안에서 남은 자리만 채운다
// (저장소가 그 이상은 펼치지 않는다).
func sparseTermsWithHighLevel(ctx context.Context, query string, high []string, tune model.SearchTuning) model.SparseTerms {
	if !tune.HighLevelKeywordsToSparse || len(high) == 0 ||
		(tune.SparseQuery != model.SparseQueryChunk && tune.SparseQuery != model.SparseQueryChunkDoc) {
		return sparseTermsFor(ctx, query, tune)
	}

	terms := sparseq.Extract(query)
	baseTS, baseLike := len(terms.TS), len(terms.Like)
	for _, phrase := range high {
		extra := sparseq.Extract(phrase)
		terms.TS = appendDistinct(terms.TS, extra.TS, sparseq.MaxTerms)
		terms.Like = appendDistinct(terms.Like, extra.Like, sparseq.MaxTerms)
	}
	st := model.SparseTerms{TSQuery: sparseq.TSQuery(terms.TS), Like: terms.Like}
	slog.DebugContext(ctx, "search: sparse query terms extracted",
		"mode", tune.SparseQuery,
		"terms_version", sparseq.Version,
		"ts_terms", len(terms.TS),
		"like_terms", len(terms.Like),
		"high_level_ts_added", len(terms.TS)-baseTS,
		"high_level_like_added", len(terms.Like)-baseLike,
		"raw_fallback", !st.Active())
	return st
}

// appendDistinct 는 src 를 대소문자 무시 중복 없이 dst 뒤에 붙이되 limit 개를
// 넘기지 않는다.
func appendDistinct(dst, src []string, limit int) []string {
	for _, v := range src {
		if len(dst) >= limit {
			return dst
		}
		dup := false
		for _, have := range dst {
			if strings.EqualFold(have, v) {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, v)
		}
	}
	return dst
}
