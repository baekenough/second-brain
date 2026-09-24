// Package sparseq 는 문장형 질의에서 희소(sparse) 레인용 키워드를 뽑는다(#276).
//
// 배경: 문서·청크의 tsvector 는 'simple' 설정이라 한국어 어절이 조사까지 붙은
// 하나의 렉심으로 남는다("회의를"). 그래서 "이번 주 회의 일정 알려줘" 같은
// 문장 질의를 plainto_tsquery(AND)로 보내면 "알려줘" 까지 전부 일치해야 해서
// 사실상 0건이 되고, bigm 레인의 LIKE '%질문 전체%' 도 마찬가지로 0건이다.
// 이 패키지는 질문에서 시간 표현·불용어·서술어 어미·조사를 걷어 낸 키워드를
// 만들어, 저장소가 접두 OR tsquery('회의':* | '일정':*)와 키워드별 LIKE 로
// 검색할 수 있게 한다.
//
// 설계 제약:
//   - 표준 라이브러리만 쓴다(형태소 분석기·LLM 호출 없음). 같은 입력에는
//     항상 같은 출력이 나온다 — 평가 baseline 비교가 이 결정론에 기댄다.
//   - 어휘 목록(시간 표현·불용어·어미·조사)은 코드다. 목록을 바꾸면 검색
//     결과가 바뀌므로 Version 을 올려 새 baseline 계열을 시작해야 한다.
//   - 추출 결과는 질문에서 파생된 개인 데이터다. 로그·trace·덤프·에러
//     문자열·메트릭 라벨에 절대 싣지 말고 개수만 남긴다.
package sparseq

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Version 은 추출 어휘의 판이다. 시간 표현·불용어·어미·조사 목록이나 분리
// 규칙을 바꾸면 반드시 올린다. 평가 실행 프로필(cmd/eval 의
// sparse_terms_version)에 실려, 어휘가 다른 실행이 같은 baseline 계열로
// 섞이지 않게 한다. 올릴 때는 sparseq_test.go 의 lexiconDigests 에 새 판의
// 어휘 지문을 추가해야 한다 — 어휘만 바꾸고 판을 그대로 두면 그 테스트가
// 실패한다.
//
// 판 이력:
//   - v1: 최초(#276).
//   - v2: 불용어 "중에"·"이건"·"그건"·"저건" 추가(PR-B 리뷰 후속).
const Version = "v2"

// MaxTerms 는 키워드 수 상한이다. 저장소가 LIKE 키워드마다 플레이스홀더를
// 하나씩 OR 로 펼치므로(pg_bigm GIN 은 LIKE ANY(array)를 인덱스로 못 쓴다)
// 이 값이 곧 SQL 조건 개수의 상한이다. TS·Like 각각에 적용된다.
const MaxTerms = 8

// minTermRunes 는 키워드 최소 길이(rune)다. 한 글자 한국어 키워드는 접두
// 매치로 코퍼스 대부분을 끌어오고, pg_bigm 도 2-gram 미만은 인덱스를 못 쓴다.
const minTermRunes = 2

// Terms 는 추출 결과다. 둘 다 비어 있으면(Empty) 호출자는 기존 raw 질의
// 경로를 그대로 쓴다.
type Terms struct {
	// TS 는 to_tsquery 용 렉심이다. 소문자이고 [\p{L}\p{N}]+ 만으로
	// 이루어지며 2 rune 이상이다 — tsquery 연산자(& | ! ( ) : * < - > ' \)가
	// 들어갈 수 없다.
	TS []string
	// Like 는 LIKE / pg_bigm 용 부분 문자열이다. 2 rune 이상이고, 이메일·
	// 전화번호·버전처럼 '@' '.' '-' '_' 가 안에 든 정확 토큰은 한 덩어리로
	// 남는다. 대소문자는 사용자가 입력한 그대로다 — LIKE 는 대소문자를
	// 구분하고, raw 경로도 입력 그대로 비교하므로 같은 규칙을 따른다.
	Like []string
}

// Empty 는 살아남은 키워드가 하나도 없는지 알린다.
func (t Terms) Empty() bool { return len(t.TS) == 0 && len(t.Like) == 0 }

// Extract 는 질문에서 키워드를 뽑는다. 처리 순서:
//
//  1. 정규화: 풀어쓴 한글 자모(NFD)를 완성형으로 합친다.
//  2. 시간 표현 제거 — 기간은 이미 OccurredFrom/To 창이 제약하므로 어휘
//     조건으로는 잡음("이번", "주")일 뿐이다.
//  3. 공백·구두점으로 분리. '@' '.' '-' '_' 를 안에 품은 조각은 정확 토큰.
//  4. 불용어(요청 동사·의문사) 제거.
//  5. 서술어 어미 제거(3 rune 이상 토큰만): 어간이 2 rune 이상이면 남기고
//     아니면 토큰을 버린다.
//  6. 끝 조사 하나 제거(가장 긴 것부터). 남는 길이가 2 rune 이상일 때만.
//  7. 2 rune 미만 제거, 첫 등장 순서 유지 중복 제거, MaxTerms 로 자르기.
//
// 아무것도 남지 않으면 빈 Terms 를 돌려준다.
func Extract(q string) Terms {
	s := composeHangul(q)
	s = timeExprRe.ReplaceAllString(s, " ")

	var out Terms
	seenLike := make(map[string]struct{})
	seenTS := make(map[string]struct{})
	for _, piece := range splitPieces(s) {
		if len(out.Like) >= MaxTerms {
			break
		}
		like, ts, ok := analyze(piece)
		if !ok {
			continue
		}
		key := strings.ToLower(like)
		if _, dup := seenLike[key]; dup {
			continue
		}
		seenLike[key] = struct{}{}
		out.Like = append(out.Like, like)
		for _, t := range ts {
			if len(out.TS) >= MaxTerms {
				break
			}
			if _, dup := seenTS[t]; dup {
				continue
			}
			seenTS[t] = struct{}{}
			out.TS = append(out.TS, t)
		}
	}
	return out
}

// TSQuery 는 TS 렉심을 접두 OR tsquery 문자열로 만든다:
//
//	'회의':* | '일정':*
//
// 결과는 반드시 하나의 바인딩 파라미터로 to_tsquery 에 넘겨야 하며 SQL
// 본문에 이어 붙이면 안 된다. 방어를 한 겹 더 둔다: [\p{L}\p{N}]+ 가 아닌
// 항목은 버리고, 작은따옴표는 두 번 쓴다(앞의 필터 때문에 실제로는 도달하지
// 않는다). 남는 항목이 없으면 빈 문자열이다.
func TSQuery(ts []string) string {
	var b strings.Builder
	n := 0
	for _, t := range ts {
		if n == MaxTerms {
			break
		}
		if !isLexeme(t) {
			continue
		}
		if n > 0 {
			b.WriteString(" | ")
		}
		b.WriteByte('\'')
		b.WriteString(strings.ReplaceAll(t, "'", "''"))
		b.WriteString("':*")
		n++
	}
	return b.String()
}

// isLexeme 은 s 가 비어 있지 않고 글자·숫자로만 이루어졌는지 본다.
func isLexeme(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isWordRune(r) {
			return false
		}
	}
	return true
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }

// isConnector 는 정확 토큰(이메일·전화번호·버전·파일명) 안에서만 의미가 있는
// 연결 문자다. 조각의 앞뒤에 붙은 것은 잘라 낸다.
func isConnector(r rune) bool { return r == '@' || r == '.' || r == '-' || r == '_' }

// splitPieces 는 공백과, 글자·숫자·연결 문자가 아닌 모든 문자에서 자른다.
// 조각 앞뒤의 연결 문자는 떼어 낸다("회의." → "회의").
func splitPieces(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !isWordRune(r) && !isConnector(r)
	})
	out := fields[:0]
	for _, f := range fields {
		f = strings.TrimFunc(f, isConnector)
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// analyze 는 조각 하나를 (Like 키워드, TS 렉심들, 채택 여부)로 바꾼다.
func analyze(piece string) (string, []string, bool) {
	if strings.IndexFunc(piece, isConnector) >= 0 {
		return analyzeExact(piece)
	}
	w := piece
	if isStopword(w) {
		return "", nil, false
	}
	var ok bool
	if w, ok = stripPredicate(w); !ok || isStopword(w) {
		return "", nil, false
	}
	w = stripParticle(w)
	if isStopword(w) || utf8.RuneCountInString(w) < minTermRunes {
		return "", nil, false
	}
	ts := lexemeRuns(w)
	return w, ts, true
}

// analyzeExact 는 연결 문자를 품은 정확 토큰을 다룬다. 불용어·어미 규칙은
// 적용하지 않고, 라틴·숫자 뒤에 붙은 한국어 조사만 뗀다
// ("010-1234-5678로" → "010-1234-5678").
func analyzeExact(piece string) (string, []string, bool) {
	w := strings.TrimFunc(stripBoundaryParticle(piece), isConnector)
	if utf8.RuneCountInString(w) < minTermRunes || isStopword(w) {
		return "", nil, false
	}
	return w, lexemeRuns(w), true
}

// lexemeRuns 는 소문자로 바꾼 w 에서 글자·숫자 연속 구간을 뽑아 2 rune
// 이상인 것만 돌려준다. 소문자화가 글자가 아닌 결합 문자를 만들 수 있어
// (예: 'İ' → "i̇") 구간 분리는 반드시 소문자화 뒤에 한다 — 그래야 TS 가
// 언제나 [\p{L}\p{N}]+ 를 지킨다.
func lexemeRuns(w string) []string {
	var runs []string
	for _, r := range strings.FieldsFunc(strings.ToLower(w), func(r rune) bool { return !isWordRune(r) }) {
		if utf8.RuneCountInString(r) >= minTermRunes {
			runs = append(runs, r)
		}
	}
	return runs
}

// stripPredicate 는 3 rune 이상 토큰에서 서술어 어미를 뗀다. 어미가 없으면
// 그대로(true), 어미를 뗀 어간이 2 rune 이상이면 어간(true), 어간이 그보다
// 짧으면 토큰 전체가 서술어였던 것이므로 버린다(false).
//
// 설계 문서의 원안은 "어미로 끝나면 토큰을 버린다" 였지만, 어간을 남기는
// 쪽으로 바꿨다: "통화했어" → "통화", "회의인지" → "회의" 처럼 명사+어미가
// 붙은 토큰이 흔하고, 과도하게 잘라도 TS 는 접두 매치·LIKE 는 부분 문자열
// 매치라 손해가 작다("무제한" → "무제" 도 "무제한" 에 매치된다).
func stripPredicate(w string) (string, bool) {
	n := utf8.RuneCountInString(w)
	if n < 3 {
		return w, true
	}
	for _, suf := range predicateSuffixes {
		if !strings.HasSuffix(w, suf) {
			continue
		}
		stem := strings.TrimSuffix(w, suf)
		if utf8.RuneCountInString(stem) < minTermRunes {
			return "", false
		}
		return stem, true
	}
	return w, true
}

// stripParticle 은 끝 조사 하나를 가장 긴 것부터 떼되, 남는 길이가 2 rune
// 이상일 때만 뗀다. 이 규칙이 "회의"(→ "회"), "결과", "사과", "효과" 같은
// 2 rune 명사를 지킨다. 3 rune 명사를 잘못 자르는 경우("고양이" → "고양")는
// 접두·부분 문자열 매치라 여전히 "고양이" 에 맞는다.
func stripParticle(w string) string {
	for _, p := range particles {
		if !strings.HasSuffix(w, p) {
			continue
		}
		stem := strings.TrimSuffix(w, p)
		if utf8.RuneCountInString(stem) >= minTermRunes {
			return stem
		}
		return w
	}
	return w
}

// stripBoundaryParticle 은 정확 토큰 끝에서 비한글 문자 바로 뒤에 붙은
// 한글 조사만 뗀다. "api-v2를" → "api-v2". 한글 사이의 조사 후보는 건드리지
// 않는다 — 정확 토큰 안의 한글은 이름의 일부일 수 있다.
func stripBoundaryParticle(w string) string {
	runes := []rune(w)
	i := len(runes)
	for i > 0 && unicode.Is(unicode.Hangul, runes[i-1]) {
		i--
	}
	if i == 0 || i == len(runes) {
		return w
	}
	tail := string(runes[i:])
	for _, p := range particles {
		if tail == p {
			return string(runes[:i])
		}
	}
	return w
}

// isStopword 는 토큰 전체가 불용어이거나, 띄어 쓴 조사 하나("foo@bar.com
// 에서")인지 본다.
func isStopword(w string) bool {
	if _, ok := stopwords[strings.ToLower(w)]; ok {
		return true
	}
	_, ok := particleSet[w]
	return ok
}

// 한글 자모 합성 상수(유니코드 3.12 절의 산술 합성).
const (
	hangulSBase  = 0xAC00
	hangulLBase  = 0x1100
	hangulVBase  = 0x1161
	hangulTBase  = 0x11A7
	hangulLCount = 19
	hangulVCount = 21
	hangulTCount = 28
	hangulSCount = hangulLCount * hangulVCount * hangulTCount
)

// composeHangul 은 풀어쓴 첫소리·가운뎃소리·끝소리 자모(NFD 한글)를 완성형
// 음절로 합친다. macOS 에서 온 텍스트가 대표적인 NFD 입력이다. 표준
// 라이브러리에 NFC 가 없어 한글 음절 합성만 직접 구현했다 — 이 패키지가 다루는
// 매칭 대상은 한국어라 이것으로 충분하고, 새 의존성을 들이지 않는다.
func composeHangul(s string) string {
	hasJamo := false
	for _, r := range s {
		if r >= hangulLBase && r <= 0x11FF {
			hasJamo = true
			break
		}
	}
	if !hasJamo {
		return s
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if n := len(out); n > 0 {
			last := out[n-1]
			if last >= hangulLBase && last < hangulLBase+hangulLCount &&
				r >= hangulVBase && r < hangulVBase+hangulVCount {
				out[n-1] = hangulSBase + ((last-hangulLBase)*hangulVCount+(r-hangulVBase))*hangulTCount
				continue
			}
			if last >= hangulSBase && last < hangulSBase+hangulSCount && (last-hangulSBase)%hangulTCount == 0 &&
				r > hangulTBase && r < hangulTBase+hangulTCount {
				out[n-1] = last + (r - hangulTBase)
				continue
			}
		}
		out = append(out, r)
	}
	return string(out)
}

// timeExprRe 는 internal/intent 의 기간 정규식이 인식하는 표현을 모두 덮는다
// (intent 패키지의 커버리지 테스트가 이를 강제한다). 대안(|)은 앞에서부터
// 시도되므로 숫자를 품은 긴 표현이 짧은 표현보다 먼저 와야 한다 — "최근 3일"
// 에서 "최근" 만 지우고 "3일" 을 키워드로 남기지 않기 위해서다.
//
// intent 에 의존하지 않고 목록을 따로 두는 이유: search 가 단어 목록 하나
// 때문에 LLM 클라이언트까지 끌고 오는 intent 를 임포트하게 되기 때문이다.
var timeExprRe = regexp.MustCompile(strings.Join([]string{
	`\d{4}(?:년\s*|-)\d{1,2}(?:월\s*|-)\d{1,2}일?`,
	`(?:지난|최근)\s*\d+\s*(?:일|주|달|개월)`,
	`\d+\s*(?:일|주|달|개월)\s*전`,
	`\d{4}년\s*\d{1,2}월(?:\s*\d{1,2}일)?`,
	`\d{1,2}월(?:\s*\d{1,2}일)?`,
	`(?:이번|지난|저번|다음)\s*주\s*[월화수목금토일]요일`,
	`[월화수목금토일]요일`,
	`(?:이번|지난|저번)\s*주말?`,
	`다음\s*주`,
	`(?:이번|다음|지난|저번)\s*달`,
	`그저께|그제|내일모레|모레|내일|오늘|어제`,
	`올해|작년`,
	`최근`,
}, "|"))

// predicateSuffixes 는 서술어 어미다. 가장 긴 것부터 검사한다. "한" 만
// 한 글자인데, "통화한 내용" 처럼 관형형 어미가 붙은 명사형 질문이 흔해서
// 넣었다(3 rune 이상 토큰에만 적용된다).
var predicateSuffixes = []string{
	"했더라", "었더라", "였더라", "주세요",
	"해줘", "어줘", "아줘", "려줘", "해봐", "줄래", "할래",
	"했어", "했지", "했나", "했니", "했던", "했는", "하던", "하는",
	"었지", "았지", "였지", "었어", "았어", "였어", "었나", "았나", "였나", "었니",
	"나요", "까요", "는지", "인가", "인지", "이야",
	"한",
}

// particles 는 끝 조사다. 가장 긴 것부터 검사한다(복합 조사 "에서는" 이
// "는" 보다 먼저).
var particles = []string{
	"에서부터", "으로부터",
	"에게서", "한테서", "이라고", "에서는", "에서도", "에게는", "한테는", "으로는", "까지는", "부터는",
	"이랑", "에서", "에게", "한테", "께서", "부터", "까지", "보다", "처럼", "으로", "하고",
	"에는", "에도", "와는", "과는", "와의", "과의", "로는",
	"은", "는", "이", "가", "을", "를", "에", "의", "와", "과", "도", "만", "로", "랑", "께",
}

var particleSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(particles))
	for _, p := range particles {
		m[p] = struct{}{}
	}
	return m
}()

// stopwords 는 토큰 전체가 이것이면 버리는 단어다(소문자 비교). 요청 동사,
// 의문사, 지시어, 요청에 딸려 오는 명사("정리", "요약", "확인", "목록")와
// 어미를 뗀 뒤 남는 조동사 어간("있었", "했었")을 담는다.
var stopwords = func() map[string]struct{} {
	words := []string{
		// 요청 동사
		"알려줘", "찾아줘", "보여줘", "정리해줘", "요약해줘", "말해줘", "알려줄래",
		"알려주세요", "찾아주세요", "보여주세요", "알려", "찾아", "보여",
		// 의문사
		"뭐", "뭐야", "뭐였지", "뭐지", "뭐가", "뭘", "뭐라고", "뭐였더라",
		"언제", "언제야", "어디", "어디서", "어디야", "누구", "누가", "누구야", "누구지", "누군지",
		"무엇", "무슨", "어떤", "어떻게", "어땠어", "어때", "왜",
		// 관련·내용
		"관련", "관련된", "관련해서", "내용", "대해", "대한", "대해서",
		// 조동사·보조 서술어
		"있었지", "있었어", "있었던", "있어", "있나", "있니", "있지", "없어",
		"했던", "했지", "했어", "했나", "했더라", "하지", "하나", "할까", "됐어", "됐지",
		"있었", "없었", "했었", "되었",
		// 두 글자짜리 서술어 조각(3 rune 미만이라 어미 규칙이 닿지 않는다)
		"하는", "하고", "해줘", "해봐", "해요", "있는", "없는", "같은", "같이",
		// 지시어·기타
		"좀", "그", "이", "저", "것", "거", "건", "및", "등",
		"이거", "그거", "저거", "이건", "그건", "저건", "이것", "그것", "저것", "이런", "그런", "저런",
		"여기", "거기", "어느", "얼마", "뭔가",
		// "~ 중에" 는 범위를 좁히는 말일 뿐 매칭할 명사가 아니다. 조사 규칙은
		// "중에" → "중"(1 rune)을 거부해 토큰을 그대로 남기므로 불용어로 막는다.
		"중에",
		"우리", "저희", "혹시", "다시", "전부", "모두", "전체",
		// 요청에 딸려 오는 명사
		"정리", "요약", "확인", "목록",
		// 영어
		"the", "a", "an", "of", "to", "in", "on", "for", "and", "or", "is", "are", "was", "were",
		"what", "when", "where", "who", "how", "about", "please", "show", "me", "find",
		"my", "i", "did", "do", "does", "with", "from", "at", "by", "it", "this", "that",
	}
	m := make(map[string]struct{}, len(words))
	for _, w := range words {
		m[w] = struct{}{}
	}
	return m
}()
