package scheduler

import (
	"fmt"
	"strings"

	"github.com/baekenough/second-brain/internal/collector/smsmap"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/timeutil"
)

// 청크 임베딩 입력에 붙이는 문맥 헤더 길이 상한(룬 기준).
//
// 청크 본문은 그대로 두고 헤더만 앞에 얹기 때문에, 헤더가 길어질수록 청크
// 본문의 의미가 벡터에서 희석된다. SMS 청크 평균 길이가 191자인 점을 감안해
// 헤더 전체를 200자로 묶고, 그 안에서 제목·참여자를 각각 더 짧게 자른다.
const (
	chunkHeaderMaxRunes  = 200
	headerTitleMaxRunes  = 80
	headerPeopleMaxRunes = 60
)

// 소스 타입별 한글 라벨. 임베딩 모델이 한국어 질의("메일", "통화 녹음")와
// 같은 어휘 공간에서 만나도록 영어 식별자 대신 한글 라벨을 넣는다.
// 목록에 없는 소스는 source_type 문자열을 그대로 쓴다(라벨 누락이 헤더 자체를
// 없애지는 않도록).
var chunkHeaderSourceLabels = map[model.SourceType]string{
	model.SourceGmail:          "메일",
	model.SourceSMS:            "문자",
	model.SourceCall:           "통화",
	model.SourceCallLog:        "통화",
	model.SourceCallTranscript: "통화 전사",
	model.SourceCalendar:       "일정",
	model.SourceNote:           "노트",
	model.SourceAgentNote:      "에이전트 노트",
	model.SourceInsight:        "통찰",
	model.SourceSlack:          "슬랙",
	model.SourceDiscord:        "디스코드",
	model.SourceTelegram:       "텔레그램",
	model.SourceGitHub:         "깃허브",
	model.SourceNotion:         "노션",
	model.SourceGDrive:         "구글드라이브",
	model.SourceFilesystem:     "파일",
	model.SourceUpload:         "업로드",
}

// 요일 한 글자 표기. time.Weekday() 값(일=0)을 그대로 인덱스로 쓴다.
var koreanWeekdays = [7]string{"일", "월", "화", "수", "목", "금", "토"}

// BuildChunkContextHeader 는 문서 메타데이터로부터 청크 임베딩 입력 앞에 붙일
// 문맥 헤더를 결정론적으로 만든다. 같은 문서에 대해 항상 같은 문자열을
// 돌려주므로(시계·맵 순회 순서에 의존하지 않는다) 재임베딩 결과가 흔들리지
// 않는다.
//
// 형식: "[메일] 2026-09-21(일) · 제목: ... · 보낸사람: ... · 받는사람: ..."
//
// 왜 필요한가: 문서 임베딩은 title+content 를 쓰지만 청크 임베딩은 청크 본문만
// 썼다. 그래서 문서 두 번째 이후 청크는 "누가·언제·무슨 제목" 이라는, 메일·
// 문자·통화 검색에서 가장 결정적인 단서를 잃은 채로 벡터가 만들어졌다.
//
// 개인정보 취급: 전화번호(metadata["number"])는 절대 넣지 않는다. 사람 식별은
// 이름/표시명(contact_name, display_name, From/To 헤더)이 있을 때만 하고,
// 값이 [REDACTED] 이거나 숫자·기호뿐이면(= 이름이 아니라 번호) 버린다.
//
// 반환값이 빈 문자열이면 붙일 문맥이 하나도 없다는 뜻이므로 호출 측은 청크
// 본문만 임베딩하면 된다.
func BuildChunkContextHeader(doc model.Document) string {
	var parts []string

	if doc.OccurredAt != nil && !doc.OccurredAt.IsZero() {
		t := doc.OccurredAt.In(timeutil.KST())
		parts = append(parts, fmt.Sprintf("%s(%s)",
			t.Format("2006-01-02"), koreanWeekdays[int(t.Weekday())]))
	}
	if title := truncateRunes(collapseSpaces(doc.Title), headerTitleMaxRunes); title != "" {
		parts = append(parts, "제목: "+title)
	}
	parts = append(parts, chunkHeaderParticipants(doc)...)

	// 소스 라벨은 구분자 없이 맨 앞에 붙인다("[메일] 2026-09-21(월) · 제목: …").
	head := ""
	if label := chunkHeaderSourceLabel(doc.SourceType); label != "" {
		head = "[" + label + "]"
	}
	body := strings.Join(parts, " · ")
	switch {
	case head == "" && body == "":
		return ""
	case head == "":
		return truncateRunes(body, chunkHeaderMaxRunes)
	case body == "":
		return truncateRunes(head, chunkHeaderMaxRunes)
	}
	return truncateRunes(head+" "+body, chunkHeaderMaxRunes)
}

// chunkHeaderSourceLabel 은 소스 타입의 한글 라벨을 돌려준다. 매핑에 없으면
// source_type 원문을 그대로 쓴다.
func chunkHeaderSourceLabel(st model.SourceType) string {
	if label, ok := chunkHeaderSourceLabels[st]; ok {
		return label
	}
	return string(st)
}

// chunkHeaderParticipants 는 소스별 metadata 키에서 사람 정보를 뽑아
// "라벨: 값" 형태의 조각들을 만든다. 키 이름은 각 수집기가 실제로 쓰는 것과
// 일치해야 한다(internal/collector/gmail.go, collector/smsmap/smsmap.go,
// collector/calendar.go 참고). 키가 바뀌면 여기도 함께 고칠 것.
func chunkHeaderParticipants(doc model.Document) []string {
	meta := doc.Metadata
	if len(meta) == 0 {
		return nil
	}

	var parts []string
	add := func(label, value string) {
		if v := truncateRunes(sanitizePerson(value), headerPeopleMaxRunes); v != "" {
			parts = append(parts, label+": "+v)
		}
	}

	switch doc.SourceType {
	case model.SourceGmail:
		// gmail.go: metadata["from"], metadata["to"] 는 원본 헤더 문자열
		// (예: `"홍길동" <hong@example.com>`). 이름과 주소 모두 검색 단서라
		// 둘 다 남긴다.
		add("보낸사람", metaString(meta, "from"))
		add("받는사람", metaString(meta, "to"))

	case model.SourceSMS, model.SourceCall, model.SourceCallLog, model.SourceCallTranscript:
		// smsmap.go: metadata["contact_name"] 는 주소록 표시명(없을 수 있다).
		// metadata["number"] 는 원본 전화번호이므로 의도적으로 쓰지 않는다.
		add("상대", metaString(meta, "contact_name"))
		if dir := metaString(meta, "direction"); dir != "" {
			parts = append(parts, "방향: "+collapseSpaces(dir))
		}

	case model.SourceCalendar:
		// calendar.go: metadata["organizer"] 는 이메일 문자열,
		// metadata["attendees"] 는 {email, display_name, ...} 목록.
		add("주최", metaString(meta, "organizer"))
		add("참석", strings.Join(calendarAttendees(meta), ", "))
		add("장소", metaString(meta, "location"))
	}

	return parts
}

// calendarAttendees 는 metadata["attendees"] 에서 표시명(없으면 이메일)을
// 입력 순서대로 뽑는다. DB 를 거치면 []map[string]any 가 []any 로 바뀌므로
// 두 형태를 모두 받는다.
func calendarAttendees(meta map[string]any) []string {
	raw, ok := meta["attendees"]
	if !ok {
		return nil
	}

	var entries []map[string]any
	switch v := raw.(type) {
	case []map[string]any:
		entries = v
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				entries = append(entries, m)
			}
		}
	default:
		return nil
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := sanitizePerson(metaString(e, "display_name"))
		if name == "" {
			name = sanitizePerson(metaString(e, "email"))
		}
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// metaString 은 metadata 값을 문자열로 읽는다. 문자열이 아니면 빈 값.
func metaString(meta map[string]any, key string) string {
	if v, ok := meta[key].(string); ok {
		return v
	}
	return ""
}

// sanitizePerson 은 사람 이름 후보를 헤더에 넣어도 되는지 판정하고 정리한다.
// 빈 값, 가려진 값([REDACTED]), 숫자·기호뿐인 값(= 이름이 아니라 전화번호)은
// 버린다.
func sanitizePerson(s string) string {
	// 따옴표는 양끝뿐 아니라 중간에도 남는다(예: `"홍길동" <a@b.com>` 의
	// 닫는 따옴표). 전부 제거한 뒤 공백을 다시 접는다.
	s = collapseSpaces(strings.NewReplacer(`"`, " ", "'", " ").Replace(s))
	if s == "" || s == smsmap.PIIRedactionToken {
		return ""
	}
	if strings.Contains(s, smsmap.PIIRedactionToken) {
		return ""
	}
	if looksLikePhoneNumber(s) {
		return ""
	}
	return s
}

// looksLikePhoneNumber 는 문자열이 전화번호처럼 숫자와 구분기호로만 이루어져
// 있는지 본다. 이름 없는 연락처는 수집기가 원본 번호를 contact 대체값으로
// 쓰기 때문에, 그런 값이 헤더로 새어 나가는 것을 막는 마지막 방어선이다.
func looksLikePhoneNumber(s string) bool {
	digits := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '+' || r == '-' || r == '(' || r == ')' || r == ' ' || r == '.':
			// 번호에 흔히 섞이는 구분기호는 통과.
		default:
			return false
		}
	}
	return digits > 0
}

// collapseSpaces 는 개행·탭을 포함한 연속 공백을 공백 하나로 줄이고 양끝을
// 다듬는다. 헤더가 여러 줄로 번지면 청크 경계가 모호해지기 때문이다.
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateRunes 는 룬 기준으로 자른다(바이트 기준으로 자르면 한글이 깨진다).
// 잘린 경우에만 말줄임표를 붙이며, 말줄임표까지 포함해 상한을 넘지 않는다.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

// withChunkContextHeader 는 청크 임베딩에 실제로 보낼 텍스트를 만든다.
// 헤더가 비어 있으면 청크 본문을 그대로 돌려주므로, 붙일 문맥이 없을 때
// 앞머리에 빈 줄만 생기는 일이 없다.
//
// 반환값은 임베딩 입력 전용이다 — 저장되는 chunk.Content 에는 절대 쓰지 말 것.
func withChunkContextHeader(doc model.Document, chunkContent string) string {
	header := BuildChunkContextHeader(doc)
	if header == "" {
		return chunkContent
	}
	return header + "\n\n" + chunkContent
}
