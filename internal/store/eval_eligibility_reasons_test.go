package store

import "testing"

// 한 문서가 여러 부적격 사유에 동시에 해당할 수 있으므로 판정 우선순위가
// 고정돼 있어야 한다. 우선순위가 흔들리면 같은 코퍼스에서도 사유별 집계가
// 실행마다 달라져, "라벨이 가리키는 문서가 사라졌다"와 "코퍼스 정책상
// 제외했다"를 구분하려던 목적 자체가 무너진다.
func TestExclusionReasonPrecedenceIsFixed(t *testing.T) {
	for _, tc := range []struct {
		name string
		fact EvalLabelFact
		want string
	}{
		{"적격", EvalLabelFact{Found: true, Status: "active", SourceType: "gmail"}, ""},
		{"행 없음", EvalLabelFact{}, "missing"},
		{"상태가 active 가 아님", EvalLabelFact{Found: true, Status: "superseded"}, "status_not_active"},
		{"삭제 시각 있음", EvalLabelFact{Found: true, Status: "active", Deleted: true}, "deleted_at"},
		{"insight 문서", EvalLabelFact{Found: true, Status: "active", SourceType: "insight"}, "insight"},
		{"disposable 보존태그", EvalLabelFact{Found: true, Status: "active", SourceType: "gmail", Retention: "disposable"}, "disposable"},
		// 아래 두 건이 우선순위를 실제로 강제한다. 사유가 겹칠 때 더 앞선
		// 쪽으로만 세야 합계가 Relevant+Irrelevant 와 어긋나지 않는다.
		{"삭제이면서 disposable", EvalLabelFact{Found: true, Status: "active", Deleted: true, Retention: "disposable"}, "deleted_at"},
		{"비활성이면서 insight", EvalLabelFact{Found: true, Status: "archived", SourceType: "insight"}, "status_not_active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fact.exclusionReason(); got != tc.want {
				t.Fatalf("exclusionReason() = %q, want %q", got, tc.want)
			}
			if eligible := tc.fact.Eligible(); eligible != (tc.want == "") {
				t.Fatalf("Eligible() = %v but reason %q", eligible, tc.want)
			}
		})
	}
}

// 사유별 집계의 합은 제외된 라벨 수와 반드시 같아야 한다. 사유 분류를
// 늘리면서 어느 한 갈래를 빠뜨리면 총계와 내역이 조용히 어긋난다.
func TestExclusionReasonsTotalCoversEveryReason(t *testing.T) {
	reasons := EvalExclusionReasons{Missing: 1, StatusNotActive: 2, Deleted: 3, Insight: 4, Disposable: 5}
	if reasons.Total() != 15 {
		t.Fatalf("Total() = %d, want 15", reasons.Total())
	}
}
