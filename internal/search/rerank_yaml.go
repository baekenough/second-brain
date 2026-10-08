package search

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/timeutil"
)

// RerankYAMLVersion 은 RerankInputYAML 의 문서 형식 판이다. 필드 구성이나
// 렌더링 규칙을 바꾸면 올린다 — 같은 노브 값이라도 리랭커가 받는 텍스트가
// 달라지면 다른 baseline 계열이어야 한다(cmd/eval 실행 프로필에 기록).
const RerankYAMLVersion = "v1"

// buildRerankDocsYAML 은 model.RerankInputYAML 의 리랭커 입력을 만든다.
//
// Cohere 는 구조화된 문서를 YAML 문자열로 보내라고 권한다. 본문(text)은
// best_chunk 가 보내는 것과 정확히 같은 텍스트다 — 같은 청크 조회 경로
// (prefetchChunks/bestChunkFromBatch, 없으면 bestChunkText)와 같은 head 폴백.
// 다른 점은 머리글 한 줄 대신 필드를 나눠 싣는다는 것뿐이고, 문서당
// maxRerankDocRunes 예산도 같다.
//
//	source: call
//	date: 2026-09-01
//	title: "..."
//	counterpart: "홍길동"
//	text: |-
//	  본문 첫 줄
//	  본문 둘째 줄
//
// 개인정보: 리랭커는 이미 제목·본문을 받는다. 이 형식이 더하는 것은 소스
// 이름, KST 날짜(일 단위), 연락처 표시 이름뿐이다. 전화번호·number_hash·
// SourceID 는 싣지 않고, 전화번호처럼 보이는 표시 이름도 뺀다. 만든 텍스트는
// 어떤 로그에도 남기지 않는다.
func (s *Service) buildRerankDocsYAML(ctx context.Context, query string, results []*model.SearchResult) []string {
	docs := make([]string, len(results))
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
		docs[i] = truncateRunes(rerankYAMLDoc(r, body), maxRerankDocRunes)
	}
	return docs
}

// rerankYAMLDoc 은 문서 하나를 YAML 매핑 문자열로 렌더링한다. 값이 없는
// 필드(date·title·counterpart)는 줄째 생략한다. 결과는 유효한 YAML 이다:
// 한 줄 값은 큰따옴표 스칼라로 이스케이프하고, 본문은 2칸 들여쓴 literal
// block(|-)이다.
func rerankYAMLDoc(r *model.SearchResult, body string) string {
	var b strings.Builder
	st := string(model.NormalizeSourceType(r.SourceType))
	if st == "" {
		st = "document"
	}
	b.WriteString("source: ")
	b.WriteString(yamlPlainOrQuote(st))
	b.WriteByte('\n')
	if r.OccurredAt != nil {
		b.WriteString("date: ")
		b.WriteString(r.OccurredAt.In(timeutil.KST()).Format(time.DateOnly))
		b.WriteByte('\n')
	}
	if title := oneLine(r.Title); title != "" {
		b.WriteString("title: ")
		b.WriteString(yamlQuote(title))
		b.WriteByte('\n')
	}
	if name := rerankCounterpart(r); name != "" {
		b.WriteString("counterpart: ")
		b.WriteString(yamlQuote(name))
		b.WriteByte('\n')
	}
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		b.WriteString(`text: ""`)
		return b.String()
	}
	b.WriteString("text: |-")
	for _, line := range strings.Split(body, "\n") {
		b.WriteString("\n  ")
		b.WriteString(line)
	}
	return b.String()
}

// rerankCounterpart 는 연락처 표시 이름(metadata.contact_name)만 돌려준다.
// 공백을 정리하고 120 rune 으로 자른다. 전화번호처럼 보이는 값은 버린다 —
// 연락처 이름 칸에 번호가 저장된 경우가 있을 수 있고, 이 노브는 번호를
// 리랭커로 새로 내보내지 않는다는 약속을 지켜야 한다.
func rerankCounterpart(r *model.SearchResult) string {
	name, _ := r.Metadata["contact_name"].(string)
	name = oneLine(name)
	if name == "" || looksLikePhoneNumber(name) {
		return ""
	}
	return truncateRunes(name, 120)
}

// looksLikePhoneNumber 는 숫자와 전화번호 구분 기호(+ - ( ) . 공백)로만
// 이뤄지고 숫자가 3개 이상인 문자열이면 true 다.
func looksLikePhoneNumber(s string) bool {
	digits := 0
	for _, c := range s {
		switch {
		case unicode.IsDigit(c):
			digits++
		case strings.ContainsRune("+-(). ", c):
		default:
			return false
		}
	}
	return digits >= 3
}

// oneLine 은 줄바꿈·연속 공백을 공백 하나로 접는다.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// yamlPlainOrQuote 는 소문자·숫자·하이픈으로만 된 값(소스 이름)은 따옴표
// 없이, 그 밖의 값은 큰따옴표 스칼라로 쓴다.
func yamlPlainOrQuote(s string) string {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return yamlQuote(s)
		}
	}
	return s
}

// yamlQuote 는 YAML 큰따옴표 스칼라를 만든다. strconv.Quote 의 이스케이프
// (\" \\ \n \t \uXXXX)는 YAML 큰따옴표 스칼라에서도 같은 뜻이다.
func yamlQuote(s string) string {
	return strconv.Quote(s)
}
