package model

// WithRetrievalDefaults는 카카오톡 원문을 명시적으로 요청한 검색에만 포함한다.
// 짧은 메시지 코퍼스의 혼합 검색 품질을 검증하기 전에는 기본 RAG 후보로 쓰지 않는다.
// 서비스와 저장소가 함께 호출해 직접 SQL 검색도 같은 경계를 지킨다.
func (q SearchQuery) WithRetrievalDefaults() SearchQuery {
	for _, source := range q.IncludeSourceTypes() {
		if source == SourceKakao {
			return q
		}
	}
	for _, source := range q.ExcludeSourceTypes {
		if source == SourceKakao {
			return q
		}
	}
	q.ExcludeSourceTypes = append(append([]SourceType(nil), q.ExcludeSourceTypes...), SourceKakao)
	return q
}
