package scheduler

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

func TestBuildChunkContextHeader(t *testing.T) {
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
			name: "sms: 전화번호는 헤더에 넣지 않는다",
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
				t.Errorf("헤더 불일치\n got: %q\nwant: %q", got, tc.want)
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

// withChunkContextHeader 는 임베딩 입력만 조립한다. 저장되는 청크 본문은
// 호출자가 그대로 유지해야 하므로, 헤더가 없을 때 본문을 그대로 돌려주는지도
// 함께 확인한다.
func TestWithChunkContextHeader(t *testing.T) {
	doc := model.Document{
		SourceType: model.SourceSMS,
		Title:      "문자",
		Metadata:   map[string]any{"contact_name": "김영희"},
	}

	got := withChunkContextHeader(doc, "청크 본문")
	want := "[문자] 제목: 문자 · 상대: 김영희\n\n청크 본문"
	if got != want {
		t.Errorf("임베딩 입력 불일치\n got: %q\nwant: %q", got, want)
	}
	if !strings.HasSuffix(got, "\n\n청크 본문") {
		t.Errorf("청크 본문이 원형 그대로 유지돼야 한다: %q", got)
	}

	// 붙일 문맥이 전혀 없으면 본문만 돌려준다(앞머리에 빈 줄이 생기면 안 된다).
	empty := model.Document{SourceType: model.SourceType("")}
	if got := withChunkContextHeader(empty, "청크 본문"); got != "청크 본문" {
		t.Errorf("헤더가 없으면 본문 그대로여야 한다: %q", got)
	}
}

// 재임베딩 모드의 배치 크기가 평상시 백필보다 충분히 커야 한다.
//
// 근거: 청크 8.2만 건을 수집 주기(기본 10분)마다 100건씩 처리하면 827 사이클
// = 약 5.7일이 걸려 재임베딩이 사실상 불가능해진다. 이 상수를 다시 줄이려면
// 그 계산부터 다시 할 것.
func TestReembedBatchSizes_충분히_크다(t *testing.T) {
	if chunkReembedBatchSize <= chunkBackfillBatchSize {
		t.Errorf("청크 재임베딩 배치(%d)가 평상시 백필(%d) 이하다",
			chunkReembedBatchSize, chunkBackfillBatchSize)
	}
	if reembedBatchSize <= backfillBatchSize {
		t.Errorf("문서 재임베딩 배치(%d)가 평상시 백필(%d) 이하다",
			reembedBatchSize, backfillBatchSize)
	}

	// 청크 8.2만 건 기준으로 사이클 수가 200 이하인지(=10분 주기로 하루 반 이내).
	const productionChunkCount = 82_723
	if cycles := productionChunkCount / chunkReembedBatchSize; cycles > 200 {
		t.Errorf("전체 재임베딩에 %d 사이클이 필요하다(배치 %d) — 너무 느리다",
			cycles, chunkReembedBatchSize)
	}
}

// embeddingSelectorVersion 은 재임베딩이 꺼져 있으면 빈 문자열을 돌려줘야
// 한다. 이 성질이 깨지면 플래그를 켜지 않아도 전량 재임베딩이 시작된다.
func TestEmbeddingSelectorVersion(t *testing.T) {
	s := &Scheduler{chunkEmbedVersion: "m:1536:chunk-ctx-v1"}
	if got := s.embeddingSelectorVersion(s.chunkEmbedVersion); got != "" {
		t.Errorf("재임베딩이 꺼졌는데 선별 버전이 비어 있지 않다: %q", got)
	}

	s.reembedStale = true
	if got := s.embeddingSelectorVersion(s.chunkEmbedVersion); got != "m:1536:chunk-ctx-v1" {
		t.Errorf("재임베딩이 켜졌는데 선별 버전이 전달되지 않았다: %q", got)
	}

	// 버전 배선이 없으면(빈 문자열) 켜져 있어도 선별하지 않는다.
	empty := &Scheduler{reembedStale: true}
	if got := empty.embeddingSelectorVersion(empty.chunkEmbedVersion); got != "" {
		t.Errorf("버전 미배선인데 선별 버전이 생겼다: %q", got)
	}
}
