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

// MaxEntityKeywordSeeds 는 키워드가 맞힐 수 있는 엔티티(시드) 수 상한이다.
// 두 글자 키워드의 접두 일치("김민%")는 엔티티 수천 개와 맞을 수 있고, 그 수가
// 그대로 엔티티 레인의 document_entities 조인·그래프 레인의 관계 조회·허브 감쇠의
// 언급 수 집계 횟수가 된다(실측: 시드 11k 에서 문장 전체 0.5초).
const MaxEntityKeywordSeeds = 64

// entityKeywordSeeds 는 키워드에 맞는 엔티티 id 를 최대 MaxEntityKeywordSeeds 개
// 고르는 SELECT 다. 엔티티 레인·그래프 레인·결과 시드 확장이 모두 이 하나를 쓴다.
//
// 매칭은 접두 일치(LIKE ANY) 하나다 — 키워드 kw 는 언제나 자기 패턴 "kw%" 와
// 맞으므로 정확 일치(= ANY)를 WHERE 에 OR 로 더해도 결과가 같다. 정확 일치는
// 상한을 넘을 때 무엇을 남길지 정하는 ORDER BY 에만 쓴다: 정확히 맞은 이름 먼저,
// 그다음 짧은 이름(키워드에 가까운 이름), 마지막으로 id(결정론).
//
// 인덱스: LIKE 접두 일치는 비-C 콜레이션에서 btree(idx_entities_normalized_type)를
// 쓰지 못해 entities 를 순차로 읽는다. entities 는 문서 수보다 훨씬 작고 키워드가
// 최대 MaxEntityKeywords 개라 비교 비용이 한정된다 — 새 인덱스·마이그레이션은 추가하지
// 않는다. kwParam/prefixParam 은 각각 text[] 로 바인딩된 "$n" 이다(entityKeywordPrefixes).
func entityKeywordSeeds(kwParam, prefixParam string) string {
	return fmt.Sprintf(`SELECT e.id
				FROM entities e
				WHERE e.normalized_name LIKE ANY(%s::text[])
				ORDER BY (e.normalized_name = ANY(%s::text[])) DESC, char_length(e.normalized_name) ASC, e.id ASC
				LIMIT %d`, prefixParam, kwParam, MaxEntityKeywordSeeds)
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
			JOIN documents d ON d.id = de.document_id
			WHERE de.entity_id IN (
				%s
			)
			%s
			%s
			%s
			%s
			%s
			GROUP BY de.document_id, d.occurred_at
			ORDER BY rank
			LIMIT $3
		)`, entityKeywordSeeds(kwParam, prefixParam),
		statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredRangeFilter)
}

// buildGraphCTEs 는 그래프 1-hop 레인(LightRAG 의 local 검색을 Postgres 로)이다.
// WITH 절에 이어 붙는 조각을 돌려주며, 맨 앞에 쉼표와 줄바꿈이 있다 — 레인이
// 꺼진 호출자는 이 함수를 부르지 않아 SQL 이 한 글자도 달라지지 않는다.
//
//	graph_seed: 키워드와 이름이 맞는 엔티티(시드, 최대 MaxEntityKeywordSeeds 개).
//	graph_rel:  시드가 from 또는 to 인 관계(graphRelCTE 참고 — 인덱스 두 번, 관계 id 로 중복 제거).
//	graph:      그 관계의 evidence_document_id 를 SUM(confidence) 내림차순,
//	            최신 사건 시각, 문서 id 오름차순으로 순위 매긴다.
//
// 상태·소스 포함/제외·retention·occurred 필터는 엔티티 레인과 똑같이 d. 한정형으로
// 레인 안에 넣는다. 레인이 LIMIT $3 으로 잘리므로 바깥에서 거르면 범위 밖
// 문서가 후보 슬롯을 차지해 범위 안 문서를 밀어낸다(buildHybridSearchQuery 주석 참고).
// 시드 문서 자신은 일부러 넣지 않는다 — 그건 엔티티 레인의 몫이고, 이 레인의
// 역할은 "이름이 직접 나오지 않지만 관계로 이어진 문서" 를 더하는 것이다.
func buildGraphCTEs(kwParam, prefixParam, statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredRangeFilter string, hubDamping bool) string {
	if hubDamping {
		return buildDampedGraphCTEs(kwParam, prefixParam, statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredRangeFilter)
	}
	return fmt.Sprintf(`,
		graph_seed AS (
			%s
		),
		%s,
		graph AS (
			SELECT er.evidence_document_id AS id,
			       row_number() OVER (ORDER BY SUM(er.confidence) DESC, d.occurred_at DESC NULLS LAST, er.evidence_document_id ASC) AS rank
			FROM graph_rel er
			JOIN documents d ON d.id = er.evidence_document_id
			WHERE true
			%s
			%s
			%s
			%s
			%s
			GROUP BY er.evidence_document_id, d.occurred_at
			ORDER BY rank
			LIMIT $3
		)`, entityKeywordSeeds(kwParam, prefixParam), graphRelCTE,
		statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredRangeFilter)
}

// graphRelCTE 는 시드에 닿는 관계를 고른다. 예전 형태
//
//	WHERE er.from_entity_id IN (SELECT id FROM graph_seed)
//	   OR er.to_entity_id   IN (SELECT id FROM graph_seed)
//
// 는 OR 로 묶인 두 해시 서브플랜이라 시드가 둘뿐이어도 entity_relations 를 전부
// 읽었다(EXPLAIN 실측: Seq Scan on entity_relations). 여기서는 from 쪽·to 쪽을
// 따로 조인해 idx_entity_relations_from / _to 를 각각 타게 하고, UNION ALL 뒤에
// 관계 id 로 중복을 없앤다 — 양 끝점이 모두 시드인 관계(자기 루프 포함)는 두
// 가지에서 한 번씩 나오지만 집계에는 한 번만 들어가야 예전 OR 형태와 같은 합이
// 된다. 중복된 두 행은 같은 관계 행의 복사본이라 DISTINCT ON 이 어느 쪽을 남겨도
// 값이 같다.
//
// 바로 뒤의 graph CTE 는 필터 조각이 모두 "AND ..." 로 시작하므로 WHERE true 로
// 시작한다(삭제 포함 + 필터 없음 조합에서도 문법이 깨지지 않게).
const graphRelCTE = `graph_rel AS (
			SELECT DISTINCT ON (r.id) r.id, r.evidence_document_id, r.confidence, r.from_entity_id, r.to_entity_id
			FROM (
				SELECT er.id, er.evidence_document_id, er.confidence, er.from_entity_id, er.to_entity_id
				FROM graph_seed s
				JOIN entity_relations er ON er.from_entity_id = s.id
				UNION ALL
				SELECT er.id, er.evidence_document_id, er.confidence, er.from_entity_id, er.to_entity_id
				FROM graph_seed s
				JOIN entity_relations er ON er.to_entity_id = s.id
			) r
			ORDER BY r.id
		)`

// buildDampedGraphCTEs 는 GraphHubDamping 을 켠 그래프 레인이다(HippoRAG 의 노드
// 특이성). buildGraphCTEs 와 시드 매칭·WHERE·필터·LIMIT 이 같고, 순위 식만 다르다:
//
//	seed_weight(e) = 1 / ln(e + doc_mention_count(e))
//	rank 키       = SUM(confidence * seed_weight(관계의 시드 쪽 끝점))
//
// doc_mention_count 는 그 엔티티의 document_entities 행 수다. 상한으로 자른 시드
// 집합에 대해서만 LATERAL 로 센다 — idx_document_entities_entity_id 를 시드마다 한 번 타므로
// document_entities 전체를 집계하지 않는다. 언급이 0 이면 가중치 1(감쇠 없음),
// 1000 건이면 약 0.145 다. 양 끝점이 모두 시드인 관계는 둘 중 큰 가중치를 쓴다
// (GREATEST 는 NULL 을 건너뛴다) — 관계 하나가 두 번 세지지 않게 하고, 모든 시드의
// 언급이 0 이면 순위가 감쇠를 끈 레인과 같아지게 하기 위해서다.
//
// 관계 선택(graph_rel)·WHERE 는 감쇠 없는 레인과 글자 그대로 같다. graph_seed 를
// 두 번 LEFT JOIN 하는 것은 이미 고른(중복 제거된) 관계 행에 가중치를 붙이는
// 해시 조회일 뿐이다.
func buildDampedGraphCTEs(kwParam, prefixParam, statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredRangeFilter string) string {
	return fmt.Sprintf(`,
		graph_seed AS (
			SELECT c.id,
			       1.0::float8 / ln(exp(1.0::float8) + m.n) AS w
			FROM (
				%s
			) c
			CROSS JOIN LATERAL (
				SELECT count(*) AS n
				FROM document_entities de
				WHERE de.entity_id = c.id
			) m
		),
		%s,
		graph AS (
			SELECT er.evidence_document_id AS id,
			       row_number() OVER (ORDER BY SUM(er.confidence::float8 * GREATEST(gf.w, gt.w)) DESC, d.occurred_at DESC NULLS LAST, er.evidence_document_id ASC) AS rank
			FROM graph_rel er
			JOIN documents d ON d.id = er.evidence_document_id
			LEFT JOIN graph_seed gf ON gf.id = er.from_entity_id
			LEFT JOIN graph_seed gt ON gt.id = er.to_entity_id
			WHERE true
			%s
			%s
			%s
			%s
			%s
			GROUP BY er.evidence_document_id, d.occurred_at
			ORDER BY rank
			LIMIT $3
		)`, entityKeywordSeeds(kwParam, prefixParam), graphRelCTE,
		statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredRangeFilter)
}
