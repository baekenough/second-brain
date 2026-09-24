package intent_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/baekenough/second-brain/internal/intent"
	"github.com/baekenough/second-brain/internal/sparseq"
)

// TestSparseqCoversIntentTimePhrases 는 intent 가 기간으로 해석하는 표현이
// sparseq 키워드로 새어 나가지 않는지 고정한다(#276). 기간은 이미
// OccurredFrom/To 창이 제약하므로, "이번", "주말" 같은 조각이 희소 레인
// 키워드가 되면 순수한 잡음이다.
//
// 두 가지를 본다:
//
//  1. intent 정규식이 잡은 부분 문자열 자체를 Extract 하면 아무것도 남지
//     않아야 한다(sparseq 의 시간 표현 목록이 intent 목록을 덮는다).
//  2. 질문 전체에서 뽑은 키워드 중 어느 것도 그 부분 문자열의 조각을 품지
//     않아야 한다.
//
// intent 에 새 기간 정규식을 넣고 sparseq 목록에 대응 항목을 넣지 않으면
// 여기서 실패한다. 질문 목록은 DeterministicWindow 테스트 표 두 개를 그대로
// 재사용한다.
func TestSparseqCoversIntentTimePhrases(t *testing.T) {
	t.Parallel()

	var questions []string
	for _, c := range planGolden {
		if c.deterministic {
			questions = append(questions, c.question)
		}
	}
	for _, c := range relativeWindowCases {
		questions = append(questions, c.question)
	}
	if len(questions) < 30 {
		t.Fatalf("질문 표가 비정상적으로 작다(%d개) — 표 이름이 바뀌어 조용히 비어 버린 것일 수 있다", len(questions))
	}

	regexes := intent.TimePhraseRegexes()
	matched := 0
	for _, q := range questions {
		terms := sparseq.Extract(q)
		for _, re := range regexes {
			for _, m := range re.FindAllString(q, -1) {
				matched++
				if got := sparseq.Extract(m); !got.Empty() {
					t.Errorf("%q: 기간 표현 %q 가 키워드로 남았다: %+v", q, m, got)
				}
				for _, frag := range strings.Fields(m) {
					if utf8.RuneCountInString(frag) < 2 {
						continue
					}
					for _, term := range append(append([]string(nil), terms.TS...), terms.Like...) {
						if strings.Contains(term, frag) {
							t.Errorf("%q: 키워드 %q 가 기간 조각 %q 를 품고 있다", q, term, frag)
						}
					}
				}
			}
		}
	}
	// 양성 대조군: 정규식이 하나도 매치되지 않았다면 이 테스트는 아무것도
	// 검사하지 않은 채 통과한 것이다.
	if matched < len(questions) {
		t.Fatalf("기간 정규식 매치가 %d건뿐이다(질문 %d개) — 검사가 공허하게 통과했다", matched, len(questions))
	}
}
