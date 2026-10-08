package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

// 결과 시드 그래프 확장(SearchTuning.GraphExpandBoost, Graphiti edge_search /
// Hindsight)의 저장소 쪽. 검색 서비스가 융합을 마친 뒤 한 번 부른다.
//
// 입력 상한. 둘 다 서비스가 이미 지키는 값이지만, 저장소가 받는 ID 배열이
// 문장 비용을 정하므로 여기서 한 번 더 자른다.
const (
	// MaxGraphExpandSeedDocs 는 엔티티를 시드로 쓸 상위 문서 수 상한이다.
	MaxGraphExpandSeedDocs = 10
	// MaxGraphExpandCandidates 는 점수를 다시 매길 후보 수 상한이다
	// (search.overfetchLimitCap 과 같은 200).
	MaxGraphExpandCandidates = 200
)

// GraphSupportCounts 는 후보 문서마다 "시드 엔티티와 이어진 서로 다른 관계 수" 를
// 센다. 시드 엔티티 = seedDocIDs 문서들의 엔티티(document_entities) ∪
// query.EntityKeywords 로 이름이 맞는 엔티티. 관계 하나가 후보 c 를 지지한다는
// 것은 그 관계의 evidence_document_id 가 c 이고, from/to 중 한쪽이 시드라는 뜻이다.
//
// 후보 자신이 시드 문서일 때, 그 문서 자기 엔티티만으로 이어진 관계는 세지 않는다
// (s.src <> evidence). 상위 문서가 자기 관계로 자기를 올리는 순환을 막기 위해서다 —
// 다른 상위 문서나 질문 키워드가 같은 엔티티를 가리킬 때만 지지로 센다.
//
// 반환 맵에는 지지가 1 이상인 후보만 있다. 결과는 candidateIDs 의 부분집합이며
// (새 문서를 더하지 않는다), 상태·소스 포함/제외·retention·occurred 필터를
// 하이브리드 레인과 같은 d. 한정형으로 다시 건다.
func (s *DocumentStore) GraphSupportCounts(ctx context.Context, query model.SearchQuery, seedDocIDs, candidateIDs []uuid.UUID) (map[uuid.UUID]int, error) {
	sql, args, ok := buildGraphSupportQuery(query, seedDocIDs, candidateIDs)
	if !ok {
		return map[uuid.UUID]int{}, nil
	}
	rows, err := s.pg.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("graph support counts: %w", err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID]int)
	for rows.Next() {
		var (
			id uuid.UUID
			n  int64
		)
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("graph support counts scan: %w", err)
		}
		out[id] = int(n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graph support counts iter: %w", err)
	}
	return out, nil
}

// buildGraphSupportQuery 는 GraphSupportCounts 의 문장과 인자를 만든다. 시드가
// 하나도 없거나 후보가 없으면 ok=false — 질의를 보내지 않는다.
//
// 계획 의도: 관계는 evidence_document_id = ANY(후보) 로 idx_entity_relations_evidence
// 에서 고르고(최대 MaxGraphExpandCandidates 문서), 시드 쪽은 document_entities 의
// document_id 인덱스(상위 MaxGraphExpandSeedDocs 문서)와 entities 이름 매칭에서만
// 나온다. documents·임베딩·entity_relations 전체를 훑는 경로가 없다.
func buildGraphSupportQuery(query model.SearchQuery, seedDocIDs, candidateIDs []uuid.UUID) (string, []interface{}, bool) {
	if len(seedDocIDs) > MaxGraphExpandSeedDocs {
		seedDocIDs = seedDocIDs[:MaxGraphExpandSeedDocs]
	}
	if len(candidateIDs) > MaxGraphExpandCandidates {
		candidateIDs = candidateIDs[:MaxGraphExpandCandidates]
	}
	keywords := NormalizeEntityKeywords(query.EntityKeywords)
	if len(candidateIDs) == 0 || (len(seedDocIDs) == 0 && len(keywords) == 0) {
		return "", nil, false
	}
	if seedDocIDs == nil {
		seedDocIDs = []uuid.UUID{} // 빈 배열 바인딩: = ANY('{}') 는 아무것도 맞지 않는다
	}

	args := []interface{}{seedDocIDs, candidateIDs, len(candidateIDs)}
	args, filters := qualifiedDocFilters(args, query)

	keywordSeed := ""
	if len(keywords) > 0 {
		kwParam := fmt.Sprintf("$%d", len(args)+1)
		prefixParam := fmt.Sprintf("$%d", len(args)+2)
		args = append(args, keywords, entityKeywordPrefixes(keywords))
		keywordSeed = fmt.Sprintf(`
			UNION ALL
			SELECT e.id, NULL::uuid
			FROM entities e
			WHERE %s`, entityKeywordMatch("e", kwParam, prefixParam))
	}

	sql := fmt.Sprintf(`
		WITH seed AS (
			SELECT de.entity_id, de.document_id AS src
			FROM document_entities de
			WHERE de.document_id = ANY($1::uuid[])%s
		)
		SELECT er.evidence_document_id AS id, COUNT(*) AS n
		FROM entity_relations er
		JOIN documents d ON d.id = er.evidence_document_id
		WHERE er.evidence_document_id = ANY($2::uuid[])
		  AND EXISTS (
			SELECT 1 FROM seed s
			WHERE (s.entity_id = er.from_entity_id OR s.entity_id = er.to_entity_id)
			  AND (s.src IS NULL OR s.src <> er.evidence_document_id)
		  )
		%s
		GROUP BY er.evidence_document_id
		ORDER BY n DESC, er.evidence_document_id ASC
		LIMIT $3`, keywordSeed, strings.Join(filters, "\n\t\t"))
	return sql, args, true
}

// qualifiedDocFilters 는 하이브리드 레인과 같은 문서 필터를 d. 한정형으로 만든다
// (buildHybridSearchQuery 의 entity*Filter 변수들과 같은 규칙·같은 헬퍼).
func qualifiedDocFilters(args []interface{}, query model.SearchQuery) ([]interface{}, []string) {
	var filters []string
	if !query.IncludeDeleted {
		filters = append(filters, "AND d.status = 'active'")
	}
	if include := query.IncludeSourceTypes(); len(include) > 0 {
		filters = append(filters, fmt.Sprintf("AND d.source_type = ANY($%d)", len(args)+1))
		args = append(args, include)
	}
	if len(query.ExcludeSourceTypes) > 0 {
		filters = append(filters, fmt.Sprintf("AND d.source_type <> ALL($%d)", len(args)+1))
		args = append(args, query.ExcludeSourceTypes)
	}
	var retention, occurred string
	args, _, retention = appendRetentionFilter(args, query.ExcludeRetention)
	if retention != "" {
		filters = append(filters, retention)
	}
	args, _, occurred = appendOccurredRangeFilters(args, query.OccurredFrom, query.OccurredTo)
	if occurred != "" {
		filters = append(filters, occurred)
	}
	return args, filters
}
