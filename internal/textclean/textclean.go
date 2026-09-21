// Package textclean은 청킹 직전에 적용되는 소스별 텍스트 전처리를 모아둔다.
//
// 여기서 하는 정리는 documents.content 컬럼(수집기가 저장한 원문)을 건드리지
// 않는다 — internal/chunker.Split 이 문서를 청크로 쪼개기 직전에만 호출되므로,
// 재인덱스(re-index) 한 번으로 기존에 수집된 문서에도 곧바로 적용된다. 원문은
// 그대로 보존되므로 언제든 정리 규칙을 되돌리거나 바꿔도 데이터 손실이 없다.
package textclean

// sourceTypeGmail은 model.SourceGmail의 문자열 값과 일치해야 한다. 여기서
// internal/model을 직접 import하지 않는 이유는, 이 패키지가 청킹 파이프라인의
// 최하단(의존성 없는 리프 패키지)으로 남아 향후 어떤 패키지에서도 자유롭게
// 재사용할 수 있게 하기 위함이다 — 값은 문자열 리터럴로 고정해 별도 관리한다.
const sourceTypeGmail = "gmail"

// CleanForChunking은 internal/chunker가 청킹 직전에 호출하는 단일 진입점이다.
// sourceType은 model.SourceType을 string으로 변환한 값이다. 지원하지 않는
// 소스 타입은 원문을 그대로 반환한다(무변경이 기본값).
//
// 새 소스에 전처리를 추가하려면 이 스위치에 분기를 추가하면 된다.
func CleanForChunking(sourceType string, s string) string {
	switch sourceType {
	case sourceTypeGmail:
		return CleanEmailBody(s)
	default:
		return s
	}
}
