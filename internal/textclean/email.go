package textclean

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// 정리 결과가 원문 대비 아래 기준보다 짧아지면 오탐(false positive)으로 보고
// 원문을 그대로 반환한다. 뉴스레터나 이미 짧은 메일에서 인용 마커를 잘못
// 감지해 본문 전체를 날려버리는 사고를 막기 위한 안전장치다.
const (
	minKeepRatio = 0.15 // 원문 대비 최소 보존 비율(15%)
	minKeepChars = 100  // 정리 결과가 이보다 짧으면 무조건 원문 반환
)

var (
	// 영문 Gmail/Apple Mail류 답장 헤더: "On Mon, Jan 5, 2026 at 3:04 PM John Doe <j@x.com> wrote:"
	reOnWrote = regexp.MustCompile(`(?i)^on\s.+\swrote:\s*$`)

	// Outlook 구형 인용 구분선: "-----Original Message-----" (대시 개수는 클라이언트마다 다름)
	reOriginalMessage = regexp.MustCompile(`(?i)^-+\s*original message\s*-+$`)

	// 전달 메일 구분선(영문/한글): "---------- Forwarded message ----------",
	// "---------- 전달된 메시지 ----------", "----- 전달된 메일 -----"
	reForwarded = regexp.MustCompile(`(?i)^-+\s*(forwarded message|전달된\s*메(?:시지|일))\s*-+$`)

	// Gmail 한글 답장 헤더: "...님이 작성:"으로 끝나는 줄.
	// 날짜·시간·이름 형식은 클라이언트마다 다르지만("2026년 3월 5일 (목) 오후
	// 3:04에 홍길동님이 작성:", "2026. 3. 5., 홍길동 <hong@x.com>님이 작성:")
	// 이 접미사는 메일 클라이언트가 자동 삽입하는 고정 문구라 일반 본문에
	// 등장할 확률이 매우 낮다.
	reKoreanWrote = regexp.MustCompile(`님이\s*작성\s*[:：]\s*$`)

	// 이메일 헤더 필드 한 줄(From/Sent/To/Subject/Cc/Date 및 한글 대응:
	// 보낸사람/받는사람/참조/제목/날짜). 단독으로는 오탐 위험이 있어
	// findQuoteBoundary에서 인접 줄에 같은 패턴이 한 번 더 나오는지로
	// 재확인한다("제목:"이라는 단어 하나만으로는 인용 블록이라 보지 않는다).
	reHeaderField = regexp.MustCompile(`(?i)^(from|sent|to|subject|cc|date|보낸\s?사람|받는\s?사람|참조|제목|날짜)\s*[:：]`)

	// "보낸사람:"/"From:"은 일반 본문에 단독으로 등장할 확률이 극히 낮으므로
	// 인접 확인 없이 단독으로도 인용 시작 신호로 취급한다.
	reFromLabelSolo = regexp.MustCompile(`(?i)^(from|보낸\s?사람)\s*[:：]`)

	// 인용 줄('>'로 시작).
	reQuotedLine = regexp.MustCompile(`^>`)

	// RFC 3676 서명 구분자: "-- " 또는 "--" 단독 줄.
	reSignatureDelim = regexp.MustCompile(`^--\s?$`)

	// 3개 이상 연속된 빈 줄을 2개로 정리한다.
	reBlankRun = regexp.MustCompile(`\n{3,}`)
)

// 모바일 클라이언트 서명 문구(영문/한글, 소문자 비교). 이 문구가 포함된 줄부터
// 끝까지 절단한다.
var mobileSignaturePhrases = []string{
	"sent from my iphone",
	"sent from my galaxy",
	"sent from my android",
	"sent from mail for windows",
	"iphone에서 보냄",
	"galaxy에서 보냄",
	"안드로이드에서 보냄",
	"outlook for ios 다운로드",
	"outlook for android 다운로드",
}

// CleanEmailBody는 Gmail 본문에서 인용된 이전 답장, 전달 헤더, 서명 블록을
// 제거해 청킹/임베딩에 실제 새로 쓴 내용만 전달되게 한다.
//
// 절대 본문 앞부분을 잃지 않도록 다음 순서로만 동작한다:
//  1. 인용 시작 마커를 찾아 그 줄부터 끝까지 통째로 잘라낸다(본문은 항상
//     마커보다 앞에 있다고 가정).
//  2. 남은 줄 중 '>' 인용 줄을 제거한다(일부 클라이언트가 최상단에도 짧은
//     인용을 섞어 넣는 경우에 대한 방어적 처리).
//  3. 서명 구분자/모바일 서명 문구를 찾아 그 줄부터 끝까지 잘라낸다.
//  4. 반복 공백 줄을 정리한다.
//  5. 안전장치: 결과가 원문의 15% 미만이거나 100자 미만이면 원문을 그대로
//     반환한다(뉴스레터·짧은 메일 보호).
func CleanEmailBody(s string) string {
	if s == "" {
		return s
	}

	normalized := strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")

	if boundary := findQuoteBoundary(lines); boundary >= 0 {
		lines = lines[:boundary]
	}

	lines = dropQuotedLines(lines)

	if boundary := findSignatureBoundary(lines); boundary >= 0 {
		lines = lines[:boundary]
	}

	// 빈 줄 정리와 앞뒤 공백 제거는 인용/서명 제거 여부와 무관하게 항상
	// 적용한다 — 정보 손실 없는 순수한 공백 정규화이기 때문이다.
	cleaned := strings.Join(lines, "\n")
	cleaned = reBlankRun.ReplaceAllString(cleaned, "\n\n")
	cleaned = strings.TrimSpace(cleaned)

	if !passesSafetyNet(s, cleaned) {
		return s
	}
	return cleaned
}

// findQuoteBoundary는 인용된 이전 답장이 시작되는 줄 번호를 찾는다. 찾지
// 못하면 -1을 반환한다. 여러 마커가 동시에 매칭될 수 있으므로 항상 더 앞쪽
// (본문에 더 가까운) 인덱스를 채택한다 — 본문을 조금이라도 더 보존하는
// 보수적인 선택이다.
func findQuoteBoundary(lines []string) int {
	boundary := -1
	consider := func(idx int) {
		if idx >= 0 && (boundary == -1 || idx < boundary) {
			boundary = idx
		}
	}

	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}

		if reOnWrote.MatchString(line) ||
			reOriginalMessage.MatchString(line) ||
			reForwarded.MatchString(line) ||
			reKoreanWrote.MatchString(line) ||
			reFromLabelSolo.MatchString(line) {
			consider(i)
			continue
		}

		if reHeaderField.MatchString(line) && hasNearbyHeaderField(lines, i) {
			consider(i)
		}
	}

	return boundary
}

// hasNearbyHeaderField는 i번째 줄 다음 최대 5줄 안에 헤더 필드 패턴이 한 번 더
// 나오는지 확인한다. 본문에 우연히 "제목:" 같은 줄이 한 번만 등장하는 경우를
// 인용 블록으로 오판하지 않기 위한 확인이다.
func hasNearbyHeaderField(lines []string, i int) bool {
	end := i + 6
	if end > len(lines) {
		end = len(lines)
	}
	for j := i + 1; j < end; j++ {
		line := strings.TrimSpace(lines[j])
		if line == "" {
			continue
		}
		if reHeaderField.MatchString(line) {
			return true
		}
	}
	return false
}

// dropQuotedLines는 '>'로 시작하는 인용 줄을 제거한다.
func dropQuotedLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if reQuotedLine.MatchString(strings.TrimSpace(l)) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// findSignatureBoundary는 서명이 시작되는 줄 번호를 찾는다("-- " 구분자 또는
// "아이폰에서 보냄" 류 모바일 서명 문구). 찾지 못하면 -1을 반환한다.
func findSignatureBoundary(lines []string) int {
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if reSignatureDelim.MatchString(line) {
			return i
		}
		lower := strings.ToLower(line)
		for _, phrase := range mobileSignaturePhrases {
			if strings.Contains(lower, phrase) {
				return i
			}
		}
	}
	return -1
}

// passesSafetyNet은 정리 결과를 채택해도 되는지 판단한다. 원문 대비 비율 또는
// 절대 길이가 기준 미만이면 false를 반환해 원문 반환을 유도한다.
func passesSafetyNet(original, cleaned string) bool {
	if cleaned == "" {
		return false
	}
	cleanedLen := utf8.RuneCountInString(cleaned)
	if cleanedLen < minKeepChars {
		return false
	}
	originalLen := utf8.RuneCountInString(original)
	if originalLen == 0 {
		return false
	}
	return float64(cleanedLen)/float64(originalLen) >= minKeepRatio
}
