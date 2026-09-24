package chunkctx

import (
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// kstTime 은 KST 기준 시각을 UTC 로 저장한 값을 만든다. 운영 컨테이너는 UTC 로
// 돌기 때문에(internal/timeutil 패키지 주석 참고) 테스트도 UTC 저장 → KST 표기
// 경로를 그대로 지난다. 개발 머신의 로컬 타임존에 기대지 않는다.
func kstTime(t *testing.T, value string) *time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("시각 파싱 실패 %q: %v", value, err)
	}
	utc := parsed.UTC()
	return &utc
}

// TestBuildChunkContextHeader_Golden 은 P0(#270) 추출의 골든 증거다.
//
// internal/scheduler.BuildChunkContextHeader 를 이 패키지로 그대로 옮긴 것이지
// 로직을 다시 쓴 게 아니다 — 아래 기대값은 추출 전 internal/scheduler 에서
// 이미 통과하던 값과 문자 그대로 같다. 이 테이블이 그대로 통과한다는 것은
// 곧 "임베딩 입력 바이트가 이전과 동일하다"는 뜻이다(같은 임베딩 버전
// chunk-ctx-v1 을 유지하는 근거 — 재임베딩을 유발하지 않는다).
//
// 대표 문서 조합: 통화(call)·문자(sms)·메일(gmail)·일정(calendar), 각각
// 참여자 있음/없음, 한국어 값 포함.
func TestBuildChunkContextHeader_Golden(t *testing.T) {
	tests := []struct {
		name string
		doc  model.Document
		want string
	}{
		{
			name: "gmail: 라벨·날짜·제목·발신자·수신자",
			doc: model.Document{
				SourceType: model.SourceGmail,
				Title:      "9월 정산 견적서 회신",
				OccurredAt: kstTime(t, "2026-09-21T01:30:00Z"), // KST 10:30
				Metadata: map[string]any{
					"from":      `"홍길동" <hong@example.com>`,
					"to":        "baek@example.com",
					"thread_id": "t-1",
				},
			},
			want: "[메일] 2026-09-21(월) · 제목: 9월 정산 견적서 회신 · 보낸사람: 홍길동 <hong@example.com> · 받는사람: baek@example.com",
		},
		{
			name: "gmail: 참여자 메타데이터 없음",
			doc: model.Document{
				SourceType: model.SourceGmail,
				Title:      "안내",
				OccurredAt: kstTime(t, "2026-09-21T01:30:00Z"), // KST 10:30
			},
			want: "[메일] 2026-09-21(월) · 제목: 안내",
		},
		{
			name: "sms: 연락처 이름과 방향",
			doc: model.Document{
				SourceType: model.SourceSMS,
				Title:      "SMS received 김영희",
				OccurredAt: kstTime(t, "2026-09-19T14:00:00Z"), // KST 23:00 같은 날
				Metadata: map[string]any{
					"contact_name": "김영희",
					"direction":    "received",
					"number":       "010-1234-5678",
				},
			},
			want: "[문자] 2026-09-19(토) · 제목: SMS received 김영희 · 상대: 김영희 · 방향: received",
		},
		{
			name: "sms: 전화번호는 헤더에 넣지 않는다(참여자 실질적 없음)",
			doc: model.Document{
				SourceType: model.SourceSMS,
				Title:      "SMS received",
				Metadata: map[string]any{
					"contact_name": "010-1234-5678",
					"number":       "010-1234-5678",
				},
			},
			want: "[문자] 제목: SMS received",
		},
		{
			name: "call: 참여자 없음",
			doc: model.Document{
				SourceType: model.SourceCall,
				Title:      "부재중 통화",
				OccurredAt: kstTime(t, "2026-09-19T14:00:00Z"), // KST 23:00 같은 날
			},
			want: "[통화] 2026-09-19(토) · 제목: 부재중 통화",
		},
		{
			name: "call: 가려진 연락처는 제외",
			doc: model.Document{
				SourceType: model.SourceCall,
				Title:      "수신 통화 [REDACTED]",
				Metadata: map[string]any{
					"contact_name": "[REDACTED]",
					"direction":    "수신",
				},
			},
			want: "[통화] 제목: 수신 통화 [REDACTED] · 방향: 수신",
		},
		{
			name: "calendar: 주최자와 참석자 표시명",
			doc: model.Document{
				SourceType: model.SourceCalendar,
				Title:      "주간 스탠드업",
				OccurredAt: kstTime(t, "2026-09-20T23:00:00Z"), // KST 2026-09-21 08:00
				Metadata: map[string]any{
					"organizer": "lead@example.com",
					"location":  "회의실 A",
					"attendees": []any{
						map[string]any{"email": "a@example.com", "display_name": "박철수"},
						map[string]any{"email": "b@example.com", "display_name": ""},
					},
				},
			},
			want: "[일정] 2026-09-21(월) · 제목: 주간 스탠드업 · 주최: lead@example.com · 참석: 박철수, b@example.com · 장소: 회의실 A",
		},
		{
			name: "calendar: 참여자 없음",
			doc: model.Document{
				SourceType: model.SourceCalendar,
				Title:      "개인 일정",
				OccurredAt: kstTime(t, "2026-09-20T23:00:00Z"), // KST 2026-09-21 08:00
			},
			want: "[일정] 2026-09-21(월) · 제목: 개인 일정",
		},
		{
			name: "calendar: 수집기가 만든 []map[string]any 형태도 처리",
			doc: model.Document{
				SourceType: model.SourceCalendar,
				Title:      "킥오프",
				Metadata: map[string]any{
					"attendees": []map[string]any{
						{"email": "a@example.com", "display_name": "박철수"},
					},
				},
			},
			want: "[일정] 제목: 킥오프 · 참석: 박철수",
		},
		{
			name: "알 수 없는 소스: source_type 을 그대로 라벨로 쓴다",
			doc: model.Document{
				SourceType: model.SourceType("weird"),
				Title:      "제목",
			},
			want: "[weird] 제목: 제목",
		},
		{
			name: "제목의 개행은 공백 하나로 접는다",
			doc: model.Document{
				SourceType: model.SourceNote,
				Title:      "여러 줄\n\n제목\t입니다",
			},
			want: "[노트] 제목: 여러 줄 제목 입니다",
		},
		{
			name: "메타데이터가 없어도 라벨은 남는다",
			doc:  model.Document{SourceType: model.SourceGmail},
			want: "[메일]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildChunkContextHeader(tc.doc)
			if got != tc.want {
				t.Errorf("헤더 불일치(추출 전후 바이트가 달라짐)\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// 헤더가 상한을 넘지 않는지, 한글이 깨지지 않는지 확인한다.
func TestBuildChunkContextHeader_상한(t *testing.T) {
	doc := model.Document{
		SourceType: model.SourceGmail,
		Title:      strings.Repeat("가", 500),
		Metadata: map[string]any{
			"from": strings.Repeat("나", 500),
			"to":   strings.Repeat("다", 500),
		},
	}

	got := BuildChunkContextHeader(doc)
	if n := len([]rune(got)); n > chunkHeaderMaxRunes {
		t.Fatalf("헤더 길이 %d 룬 > 상한 %d", n, chunkHeaderMaxRunes)
	}
	if !strings.HasPrefix(got, "[메일] 제목: ") {
		t.Fatalf("상한을 넘겨도 앞부분은 보존돼야 한다: %q", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Fatalf("룬 경계가 깨졌다: %q", got)
	}
}

// 같은 입력이면 항상 같은 헤더여야 한다(맵 순회 순서에 의존하지 않는다).
func TestBuildChunkContextHeader_결정론(t *testing.T) {
	doc := model.Document{
		SourceType: model.SourceCalendar,
		Title:      "회의",
		Metadata: map[string]any{
			"organizer": "lead@example.com",
			"location":  "회의실",
			"attendees": []any{
				map[string]any{"display_name": "가"},
				map[string]any{"display_name": "나"},
				map[string]any{"display_name": "다"},
			},
		},
	}

	first := BuildChunkContextHeader(doc)
	for i := 0; i < 50; i++ {
		if got := BuildChunkContextHeader(doc); got != first {
			t.Fatalf("반복 호출 결과가 달라졌다: %q != %q", got, first)
		}
	}
}
