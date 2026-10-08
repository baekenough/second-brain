package store

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 엔티티 키워드 상한. 질문에서 파생된 값이 SQL 조건 수와 인덱스 탐색 횟수를
// 무한정 키우지 못하게 하는 경계다. 검색 서비스(internal/search)의 LLM 추출기도
// 같은 상수로 자른다.
const (
	// MaxEntityKeywords 는 저수준 키워드 개수 상한이다. 엔티티·그래프 레인은
	// 키워드를 text[] 파라미터 하나로 묶어 보내므로 SQL 문장 크기는 늘지
	// 않지만, = ANY / LIKE ANY 의 비교 횟수가 이 값에 비례한다.
	MaxEntityKeywords = 8
	// MinEntityKeywordRunes 는 키워드 최소 길이(rune)다. 한 글자는 접두 일치로
	// 엔티티 테이블 대부분을 끌어온다.
	MinEntityKeywordRunes = 2
	// MaxEntityKeywordRunes 는 키워드 최대 길이(rune)다. 이보다 긴 항목은
	// 자르지 않고 버린다 — 잘린 이름은 다른 엔티티와 우연히 맞는다.
	MaxEntityKeywordRunes = 40
)

// NormalizeEntityKeywords 는 엔티티 레인·그래프 레인에 넘길 키워드를 정규화한다.
//
// 엔티티 이름의 저장 정규화(normalizeEntityName: 소문자 + 양끝 공백 제거)와
// 같은 규칙을 먼저 적용해야 normalized_name 과 비교가 맞는다. 그 위에 더하는
// 것은 방어 조건이다:
//
//   - 잘못된 UTF-8·제어 문자(NUL 포함)가 든 항목은 버린다. PostgreSQL 은
//     NUL 이 든 text 파라미터를 SQLSTATE 22021 로 거부해 검색 전체가 실패하는데,
//     LLM 출력이 키워드의 출처일 수 있다.
//   - MinEntityKeywordRunes 미만, MaxEntityKeywordRunes 초과 항목은 버린다.
//   - 첫 등장 순서를 유지한 채 중복을 제거하고 MaxEntityKeywords 에서 자른다.
//
// 입력 순서가 곧 우선순위다. 아무것도 남지 않으면 nil 을 돌려준다.
func NormalizeEntityKeywords(in []string) []string {
	var out []string
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		// 정규화보다 먼저 검사한다: strings.ToLower 는 잘못된 UTF-8 바이트를
		// U+FFFD 로 바꿔 버려, 정규화 뒤에 검사하면 깨진 입력이 멀쩡한 값으로
		// 통과한다.
		if !utf8.ValidString(raw) {
			continue
		}
		kw := normalizeEntityName(raw)
		if !validEntityKeyword(kw) {
			continue
		}
		if _, dup := seen[kw]; dup {
			continue
		}
		seen[kw] = struct{}{}
		out = append(out, kw)
		if len(out) == MaxEntityKeywords {
			break
		}
	}
	return out
}

func validEntityKeyword(kw string) bool {
	n := utf8.RuneCountInString(kw)
	if n < MinEntityKeywordRunes || n > MaxEntityKeywordRunes {
		return false
	}
	return !strings.ContainsFunc(kw, unicode.IsControl)
}

// entityKeywordPrefixes 는 키워드별 LIKE 접두 패턴("키워드%")을 만든다.
// 키워드 안의 LIKE 메타문자(% _ \)는 이스케이프한다 — 이름에 '_' 가 든
// 엔티티("foo_bar")가 "fooXbar" 와 맞거나, '%' 하나가 모든 엔티티와 맞는
// 일을 막는다. 기본 LIKE 이스케이프 문자가 백슬래시이므로 ESCAPE 절은 필요 없다.
func entityKeywordPrefixes(kws []string) []string {
	out := make([]string, len(kws))
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	for i, kw := range kws {
		out[i] = r.Replace(kw) + "%"
	}
	return out
}

// entityKeywordMatch 는 normalized_name 이 키워드와 맞는 조건이다. 정확 일치는
// idx_entities_normalized_type (normalized_name, type) 의 선두 컬럼을 탄다.
// 접두 일치(LIKE ANY)는 비-C 콜레이션에서 btree 를 못 쓰지만, entities 는
// 문서 수보다 훨씬 작은 테이블이고 키워드가 최대 MaxEntityKeywords 개로
// 묶여 있어 순차 비교 비용이 한정된다 — 새 인덱스·마이그레이션은 추가하지
// 않는다. kwParam/prefixParam 은 각각 text[] 로 바인딩된 "$n" 이다.
func entityKeywordMatch(alias, kwParam, prefixParam string) string {
	return fmt.Sprintf("(%[1]s.normalized_name = ANY(%[2]s::text[]) OR %[1]s.normalized_name LIKE ANY(%[3]s::text[]))",
		alias, kwParam, prefixParam)
}

// buildKeywordEntityCTE 는 키워드 모드의 엔티티 레인이다. buildEntityCTE 와
// 같은 필터(d. 한정형)를 레인 안에 넣고, 순위만 "맞은 서로 다른 엔티티 수
// 내림차순, 문서 id 오름차순" 으로 바꾼다. 질문 한 문장에서 여러 이름이
// 잡힌 문서가 한 이름만 여러 번 언급한 문서보다 앞서야 해서 COUNT(*) 가
// 아니라 COUNT(DISTINCT entity_id) 를 쓴다.
//
// 순위 동점은 최신 사건 시각(NULL 은 뒤), 그다음 문서 id 순으로 깬다 — 레인이
// LIMIT $3 으로 잘리므로 동점을 비결정적으로 두면 실행마다 후보 집합이 달라진다.
// 바깥 ORDER BY rank 는 LIMIT 이 순위 상위를 자른다는 사실을 플래너 동작에
// 맡기지 않기 위해서다.
func buildKeywordEntityCTE(kwParam, prefixParam, statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredRangeFilter string) string {
	return fmt.Sprintf(`entity AS (
			SELECT de.document_id AS id,
			       row_number() OVER (ORDER BY COUNT(DISTINCT de.entity_id) DESC, d.occurred_at DESC NULLS LAST, de.document_id ASC) AS rank
			FROM document_entities de
			JOIN entities e ON e.id = de.entity_id
			JOIN documents d ON d.id = de.document_id
			WHERE %s
			%s
			%s
			%s
			%s
			%s
			GROUP BY de.document_id, d.occurred_at
			ORDER BY rank
			LIMIT $3
		)`, entityKeywordMatch("e", kwParam, prefixParam),
		statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredRangeFilter)
}

// buildGraphCTEs 는 그래프 1-hop 레인(LightRAG 의 local 검색을 Postgres 로)이다.
// WITH 절에 이어 붙는 조각을 돌려주며, 맨 앞에 쉼표와 줄바꿈이 있다 — 레인이
// 꺼진 호출자는 이 함수를 부르지 않아 SQL 이 한 글자도 달라지지 않는다.
//
//	graph_seed: 키워드와 이름이 맞는 엔티티(시드).
//	graph:      시드가 from 또는 to 인 관계의 evidence_document_id 를
//	            SUM(confidence) 내림차순, 문서 id 오름차순으로 순위 매긴다.
//
// 상태·소스 포함/제외·retention·occurred 필터는 엔티티 레인과 똑같이 d. 한정형으로
// 레인 안에 넣는다. 레인이 LIMIT $3 으로 잘리므로 바깥에서 거르면 범위 밖
// 문서가 후보 슬롯을 차지해 범위 안 문서를 밀어낸다(buildHybridSearchQuery 주석 참고).
// 시드 문서 자신은 일부러 넣지 않는다 — 그건 엔티티 레인의 몫이고, 이 레인의
// 역할은 "이름이 직접 나오지 않지만 관계로 이어진 문서" 를 더하는 것이다.
func buildGraphCTEs(kwParam, prefixParam, statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredRangeFilter string) string {
	return fmt.Sprintf(`,
		graph_seed AS (
			SELECT e.id
			FROM entities e
			WHERE %s
		),
		graph AS (
			SELECT er.evidence_document_id AS id,
			       row_number() OVER (ORDER BY SUM(er.confidence) DESC, d.occurred_at DESC NULLS LAST, er.evidence_document_id ASC) AS rank
			FROM entity_relations er
			JOIN documents d ON d.id = er.evidence_document_id
			WHERE (er.from_entity_id IN (SELECT id FROM graph_seed)
			    OR er.to_entity_id   IN (SELECT id FROM graph_seed))
			%s
			%s
			%s
			%s
			%s
			GROUP BY er.evidence_document_id, d.occurred_at
			ORDER BY rank
			LIMIT $3
		)`, entityKeywordMatch("e", kwParam, prefixParam),
		statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredRangeFilter)
}
