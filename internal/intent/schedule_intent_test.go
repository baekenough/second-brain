package intent

import "testing"

func TestHasScheduleIntent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		q    string
		want bool
	}{
		// 일정 키워드
		{"이번 주 일정 알려줘", true},
		{"토요일 약속 있어?", true},
		{"다음 달 스케줄", true},
		// 일정 단서(키워드 없이)
		{"내일 뭐 해?", true},
		{"주말에 뭐하지", true},
		{"다음 주 미팅 언제야", true},
		{"금요일 몇 시에 만나기로 했지", true},
		{"이번 주 회의 있어?", true},
		{"병원 가야 하는 날", true},
		{"다음 달 여행 계획", true},
		{"출장 예정 날짜", true},
		// 기록 단어가 있으면 기록 질문이다
		{"어제 회의 관련 메일", false},
		{"약속 관련 문자", false},
		{"미팅 끝나고 통화한 내용", false},
		{"회의 메모 노트", false},
		{"엄마랑 한 대화에서 언제 온다고 했지", false},
		// 기록물 명사
		{"회의록 찾아줘", false},
		{"사업 계획서 파일", false},
		// 일정과 무관
		{"점심 메뉴 추천", false},
		{"프로젝트 예산 얼마", false},
		{"김철수 연락처", false},
		{"오늘 날씨", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := HasScheduleIntent(tc.q); got != tc.want {
			t.Errorf("HasScheduleIntent(%q) = %v, want %v", tc.q, got, tc.want)
		}
	}
}
