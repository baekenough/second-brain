package store

// 자동 인덱싱·추론은 아직 혼합 RAG 품질을 검증하지 않은 카카오 원문을 건너뛴다.
// 공유 HNSW에 벡터를 넣고 검색에서만 제외하면 ANN 후보와 기존 recall이 달라질 수 있다.
// 최근 목록·ID 조회·명시적인 원문 검색에는 이 정책을 적용하지 않는다.
const backgroundSourceEligibilitySQL = "source_type <> 'kakao'"
