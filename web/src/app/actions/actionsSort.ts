/** 정렬 모드와 URL 파라미터 파싱을 페이지 컴포넌트에서 분리한 순수 유틸.
 * `internal/api/actions.go`의 `sort` 쿼리 파라미터 어휘(recent|due|confidence)와
 * 일치해야 하며, 기본값도 백엔드와 같은 "recent"(최신순)다. */
export type SortMode = "recent" | "due" | "confidence";

export const DEFAULT_SORT: SortMode = "recent";

/** 알 수 없거나 없는 값은 기본 정렬("recent")로 되돌린다. */
export function parseSort(raw: string | null): SortMode {
  return raw === "due" || raw === "confidence" ? raw : DEFAULT_SORT;
}
