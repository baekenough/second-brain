package store

import (
	"context"
	"fmt"
	"time"
)

// EvalExclusions describes labels outside the default production retrieval
// corpus. Filtering affects this evaluation snapshot only, never stored labels.
type EvalExclusions struct {
	Relevant   int `json:"relevant_labels"`
	Irrelevant int `json:"irrelevant_labels"`
	Queries    int `json:"queries"`

	// Reasons 는 제외된 라벨 수를 사유별로 나눈 것이다. 합계는
	// Relevant+Irrelevant 와 항상 같다 — 한 라벨은 정확히 하나의 사유로만
	// 센다(EvalExclusionReasons 의 우선순위 참고).
	//
	// 총계만으로는 "라벨이 가리키는 문서가 사라졌다"(missing)와 "코퍼스
	// 정책상 검색 대상이 아니다"(disposable/insight)를 구분할 수 없는데,
	// 앞의 것은 데이터 결함이고 뒤의 것은 의도된 제외다.
	Reasons EvalExclusionReasons `json:"reasons"`
}

// EvalExclusionReasons 는 라벨이 평가 스냅샷에서 빠진 사유별 집계다.
//
// 한 문서가 여러 사유에 동시에 해당할 수 있으므로(예: 삭제되면서 상태도
// 바뀐 문서) 아래 필드 선언 순서를 그대로 판정 우선순위로 쓴다. 우선순위를
// 고정해 두지 않으면 같은 코퍼스에서도 집계가 흔들린다.
type EvalExclusionReasons struct {
	// Missing 은 documents 에 행 자체가 없는 경우다. 라벨은 남았는데 문서가
	// 물리적으로 지워졌다는 뜻이므로 가장 먼저 본다.
	Missing int `json:"missing"`
	// StatusNotActive 는 status<>'active' 인 경우다.
	StatusNotActive int `json:"status_not_active"`
	// Deleted 는 status 는 active 인데 deleted_at 이 채워진 경우다.
	Deleted int `json:"deleted_at"`
	// Insight 는 source_type='insight' 인 모델 생성 문서다.
	Insight int `json:"insight"`
	// Disposable 은 retention 태그가 disposable 인 문서다.
	Disposable int `json:"disposable"`
}

// Total 은 사유별 집계의 합이다.
func (r EvalExclusionReasons) Total() int {
	return r.Missing + r.StatusNotActive + r.Deleted + r.Insight + r.Disposable
}

// EvalLabelFact 은 라벨이 가리키는 문서 한 건의 적격성 판정 근거다.
//
// 제목·본문은 의도적으로 담지 않는다. 이 구조체는 평가 진단 파일로 흘러가고,
// 거기에 원문이 섞이면 산출물 전체가 개인정보가 된다.
type EvalLabelFact struct {
	Found      bool
	Status     string
	Deleted    bool
	SourceType string
	Retention  string
	OccurredAt *time.Time
}

// Eligible 은 이 문서가 기본 운영 검색 코퍼스에 속하는지를 돌려준다.
// 판정 규칙은 FilterSearchEligible 이 쓰던 SQL WHERE 절과 같다.
func (f EvalLabelFact) Eligible() bool {
	return f.Found && f.Status == "active" && !f.Deleted &&
		f.SourceType != "insight" && f.Retention != "disposable"
}

// exclusionReason 은 이 문서가 부적격인 사유를 하나 고른다. 적격이면 빈 문자열.
func (f EvalLabelFact) exclusionReason() string {
	switch {
	case !f.Found:
		return "missing"
	case f.Status != "active":
		return "status_not_active"
	case f.Deleted:
		return "deleted_at"
	case f.SourceType == "insight":
		return "insight"
	case f.Retention == "disposable":
		return "disposable"
	default:
		return ""
	}
}

// LabelFacts 는 주어진 문서 ID 들의 적격성 판정 근거를 한 번의 쿼리로 읽는다.
// 존재하지 않는 ID 는 Found=false 인 항목으로 채워 돌려주므로, 호출자는
// "행이 없다"와 "조회를 안 했다"를 구분할 수 있다.
func (s *EvalStore) LabelFacts(ctx context.Context, ids []string) (map[string]EvalLabelFact, error) {
	facts := make(map[string]EvalLabelFact, len(ids))
	for _, id := range ids {
		facts[id] = EvalLabelFact{}
	}
	if len(ids) == 0 {
		return facts, nil
	}
	rows, err := s.pg.Pool().Query(ctx, `SELECT id::text, status, deleted_at IS NOT NULL,
 source_type::text, COALESCE(metadata->>'retention',''), occurred_at
 FROM documents WHERE id=ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("eval label facts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		fact := EvalLabelFact{Found: true}
		if err := rows.Scan(&id, &fact.Status, &fact.Deleted, &fact.SourceType, &fact.Retention, &fact.OccurredAt); err != nil {
			return nil, err
		}
		facts[id] = fact
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return facts, nil
}

// FilterSearchEligible 은 기본 운영 검색 코퍼스에 없는 라벨을 이번 평가
// 스냅샷에서만 걷어내고, 걷어낸 수를 사유별로 집계해 함께 돌려준다.
// 저장된 판정 자체는 절대 고쳐 쓰지 않는다.
func (s *EvalStore) FilterSearchEligible(ctx context.Context, pairs []EvalPair) ([]EvalPair, EvalExclusions, error) {
	var excluded EvalExclusions
	ids := make([]string, 0)
	seen := map[string]bool{}
	for _, p := range pairs {
		for _, list := range [][]string{p.RelevantDocIDs, p.IrrelevantDocIDs} {
			for _, id := range list {
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
	}
	if len(ids) == 0 {
		return nil, excluded, nil
	}
	facts, err := s.LabelFacts(ctx, ids)
	if err != nil {
		return nil, excluded, err
	}
	countReason := func(id string) {
		switch facts[id].exclusionReason() {
		case "missing":
			excluded.Reasons.Missing++
		case "status_not_active":
			excluded.Reasons.StatusNotActive++
		case "deleted_at":
			excluded.Reasons.Deleted++
		case "insight":
			excluded.Reasons.Insight++
		case "disposable":
			excluded.Reasons.Disposable++
		}
	}
	filtered := make([]EvalPair, 0, len(pairs))
	for _, p := range pairs {
		positive, negative := []string{}, []string{}
		for _, id := range p.RelevantDocIDs {
			if facts[id].Eligible() {
				positive = append(positive, id)
			} else {
				excluded.Relevant++
				countReason(id)
			}
		}
		for _, id := range p.IrrelevantDocIDs {
			if facts[id].Eligible() {
				negative = append(negative, id)
			} else {
				excluded.Irrelevant++
				countReason(id)
			}
		}
		if len(positive)+len(negative) == 0 {
			excluded.Queries++
			continue
		}
		p.RelevantDocIDs, p.IrrelevantDocIDs = positive, negative
		filtered = append(filtered, p)
	}
	return filtered, excluded, nil
}
