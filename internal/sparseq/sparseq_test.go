package sparseq

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// 모든 예시는 가상 데이터다(가상 인물 "김철수"·"홍길동", 010-1234-5678 은
// 합성 번호).

func TestExtract_Table(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in       string
		wantTS   []string
		wantLike []string
	}{
		{"이번 주 회의 일정 알려줘", []string{"회의", "일정"}, []string{"회의", "일정"}},
		{"지난달 김철수랑 통화한 내용", []string{"김철수", "통화"}, []string{"김철수", "통화"}},
		{"홍길동한테 뭐라고 했더라", []string{"홍길동"}, []string{"홍길동"}},
		{"모레 회의 있나", []string{"회의"}, []string{"회의"}},
		{"프로젝트 진행 상황 어때", []string{"프로젝트", "진행", "상황"}, []string{"프로젝트", "진행", "상황"}},
		{"회의에서는 무슨 결과가 나왔지?", []string{"회의", "결과", "나왔지"}, []string{"회의", "결과", "나왔지"}},
		{"김철수와 통화했어", []string{"김철수", "통화"}, []string{"김철수", "통화"}},
		{"다음 주 수요일 일정 정리해줘", []string{"일정"}, []string{"일정"}},
		{"최근 3일 문자 확인", []string{"문자"}, []string{"문자"}},
		{"2026년 6월 예산 보고서를 보여줘", []string{"예산", "보고서"}, []string{"예산", "보고서"}},
		{"2026-08-19 회의록", []string{"회의록"}, []string{"회의록"}},
		// 영어·혼합
		{"What did Kim say about the Budget?", []string{"kim", "say", "budget"}, []string{"Kim", "say", "Budget"}},
		{"Zoom 회의 링크를 찾아줘", []string{"zoom", "회의", "링크"}, []string{"Zoom", "회의", "링크"}},
		{"API를 바꾼 이유", []string{"api", "바꾼", "이유"}, []string{"API", "바꾼", "이유"}},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := Extract(tc.in)
			if !reflect.DeepEqual(got.TS, tc.wantTS) {
				t.Errorf("TS = %q, want %q", got.TS, tc.wantTS)
			}
			if !reflect.DeepEqual(got.Like, tc.wantLike) {
				t.Errorf("Like = %q, want %q", got.Like, tc.wantLike)
			}
		})
	}
}

// TestExtract_NounsSurvive 는 조사·어미 규칙이 명사를 망가뜨리지 않는지 본다.
// 조사처럼 보이는 글자("의", "과", "이", "도")로 끝나는 2 rune 명사가 대상이다.
func TestExtract_NounsSurvive(t *testing.T) {
	t.Parallel()
	for _, noun := range []string{"회의", "결과", "사과", "효과", "의사", "가이드", "이사", "도시", "지도", "합의", "휴가", "마을", "도로", "페이지", "홈페이지"} {
		got := Extract(noun)
		if len(got.Like) != 1 || got.Like[0] != noun {
			t.Errorf("Extract(%q).Like = %q, want [%q]", noun, got.Like, noun)
		}
		if len(got.TS) != 1 || got.TS[0] != noun {
			t.Errorf("Extract(%q).TS = %q, want [%q]", noun, got.TS, noun)
		}
	}
}

// TestExtract_ParticleStripping 은 조사를 떼되 2 rune 미만이 되면 떼지 않는
// 규칙을 본다. 3 rune 명사를 과하게 자르는 경우("고양이" → "고양")도 의도된
// 동작으로 고정한다 — 접두·부분 문자열 매치라 원래 단어에 여전히 맞는다.
func TestExtract_ParticleStripping(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"회의를":     "회의",
		"일정은":     "일정",
		"김철수가":    "김철수",
		"부산으로":    "부산",
		"서버에서부터":  "서버",
		"회의에서는":   "회의",
		"고양이":     "고양",
		"선생님께":    "선생님",
		"결과를":     "결과",
		"회의록에":    "회의록",
		"보고서까지는":  "보고서",
		"노트의":     "노트",
		"김철수하고":   "김철수",
		"사과":      "사과",
		"효과가":     "효과",
		"문서들":     "문서들", // 복수 접미사는 v1 에서 다루지 않는다
		"회의인지":    "회의",
		"일정이야":    "일정",
		"진행하던":    "진행",
		"통화했던":    "통화",
		"무제한":     "무제",
		"보냈었나":    "보냈",
		"알려주세요":   "",
		"했는지":     "",
		"있었나요":    "",
		"뭐였더라":    "",
		"정리해줘":    "",
		"요약해줘":    "",
		"이번주에":    "",
		"금요일에":    "",
		"8월 15일에": "",
	}
	for in, want := range tests {
		got := Extract(in)
		switch {
		case want == "" && !got.Empty():
			t.Errorf("Extract(%q) = %+v, want empty", in, got)
		case want != "" && (len(got.Like) != 1 || got.Like[0] != want):
			t.Errorf("Extract(%q).Like = %q, want [%q]", in, got.Like, want)
		}
	}
}

// TestExtract_ExactTokens 는 이메일·전화번호·버전이 LIKE 용 한 덩어리로
// 남고, TS 에는 글자·숫자 구간만 들어가는지 본다.
func TestExtract_ExactTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in       string
		wantLike []string
		wantTS   []string
	}{
		{"foo@bar.com 에서 온 메일", []string{"foo@bar.com", "메일"}, []string{"foo", "bar", "com", "메일"}},
		{"010-1234-5678로 온 문자", []string{"010-1234-5678", "문자"}, []string{"010", "1234", "5678", "문자"}},
		{"v0.24.0 릴리스 노트", []string{"v0.24.0", "릴리스", "노트"}, []string{"v0", "24", "릴리스", "노트"}},
		{"report_final.pdf 파일", []string{"report_final.pdf", "파일"}, []string{"report", "final", "pdf", "파일"}},
		{"api-v2를 쓴 곳", []string{"api-v2"}, []string{"api", "v2"}},
		{"(회의).", []string{"회의"}, []string{"회의"}},
	}
	for _, tc := range tests {
		got := Extract(tc.in)
		if !reflect.DeepEqual(got.Like, tc.wantLike) {
			t.Errorf("Extract(%q).Like = %q, want %q", tc.in, got.Like, tc.wantLike)
		}
		if !reflect.DeepEqual(got.TS, tc.wantTS) {
			t.Errorf("Extract(%q).TS = %q, want %q", tc.in, got.TS, tc.wantTS)
		}
	}
}

func TestExtract_EmptyWhenNothingSurvives(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "   ", "뭐 있었지?", "어제 뭐 했지", "오늘", "이번 주 요약해줘", "a", "!!! ???", "😀🎉", "the what is", "올해 있었던 일 정리"} {
		if got := Extract(in); !got.Empty() {
			t.Errorf("Extract(%q) = %+v, want empty (caller falls back to raw)", in, got)
		}
	}
}

func TestExtract_CapAndDedup(t *testing.T) {
	t.Parallel()
	got := Extract("회의 회의를 일정 일정은 alpha beta gamma delta epsilon zeta eta theta iota kappa")
	if len(got.Like) != MaxTerms || len(got.TS) != MaxTerms {
		t.Fatalf("cap: Like=%d TS=%d, want %d each: %+v", len(got.Like), len(got.TS), MaxTerms, got)
	}
	want := []string{"회의", "일정", "alpha", "beta", "gamma", "delta", "epsilon", "zeta"}
	if !reflect.DeepEqual(got.Like, want) {
		t.Errorf("first-seen order: Like = %q, want %q", got.Like, want)
	}
	// 대소문자만 다른 중복은 먼저 나온 표기를 남긴다.
	got = Extract("Zoom zoom ZOOM")
	if !reflect.DeepEqual(got.Like, []string{"Zoom"}) || !reflect.DeepEqual(got.TS, []string{"zoom"}) {
		t.Errorf("case dedup: %+v", got)
	}
	// 정확 토큰 여러 개가 TS 를 MaxTerms 넘게 만들지 않는다.
	got = Extract("a1-b2-c3-d4-e5-f6-g7-h8-i9-j10 aa-bb-cc-dd-ee-ff")
	if len(got.TS) > MaxTerms {
		t.Errorf("TS cap exceeded: %d", len(got.TS))
	}
}

func TestExtract_Deterministic(t *testing.T) {
	t.Parallel()
	const q = "지난 주 김철수랑 통화한 내용 중 예산 관련 foo@bar.com 010-1234-5678 회의록 정리해줘"
	first := Extract(q)
	for i := 0; i < 100; i++ {
		if got := Extract(q); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d: %+v != %+v", i, got, first)
		}
	}
}

// TestExtract_ComposesDecomposedHangul 은 풀어쓴 자모(NFD) 입력이 완성형과
// 같은 결과를 내는지 본다.
func TestExtract_ComposesDecomposedHangul(t *testing.T) {
	t.Parallel()
	// "회의 일정" 을 첫소리·가운뎃소리·끝소리 자모로 풀어 쓴 것.
	nfd := "회의 일정"
	got := Extract(nfd)
	want := Extract("회의 일정")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NFD = %+v, NFC = %+v", got, want)
	}
	if !reflect.DeepEqual(want.TS, []string{"회의", "일정"}) {
		t.Fatalf("NFC = %+v", want)
	}
}

func TestTSQuery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"회의"}, "'회의':*"},
		{[]string{"회의", "일정"}, "'회의':* | '일정':*"},
		// 방어: 글자·숫자가 아닌 항목은 버린다.
		{[]string{"a'b", "회의", "x|y", "", "c:*", `d\`}, "'회의':*"},
		{[]string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}, "'1':* | '2':* | '3':* | '4':* | '5':* | '6':* | '7':* | '8':*"},
	}
	for _, tc := range tests {
		if got := TSQuery(tc.in); got != tc.want {
			t.Errorf("TSQuery(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// tsqueryGrammar 는 TSQuery 출력이 따라야 하는 문법이다: 비어 있거나,
// 글자·숫자만으로 된 접두 렉심들의 OR. 연산자·따옴표·역슬래시가 렉심 안에
// 들어갈 수 없다는 것이 인젝션 방어의 전부다.
var tsqueryGrammar = regexp.MustCompile(`^(?:'[\p{L}\p{N}]+':\*(?: \| '[\p{L}\p{N}]+':\*)*)?$`)

func checkTermsInvariants(t *testing.T, in string, got Terms) {
	t.Helper()
	if len(got.TS) > MaxTerms || len(got.Like) > MaxTerms {
		t.Fatalf("Extract(%q): cap exceeded TS=%d Like=%d", in, len(got.TS), len(got.Like))
	}
	for _, ts := range got.TS {
		if !isLexeme(ts) || utf8.RuneCountInString(ts) < minTermRunes || ts != strings.ToLower(ts) {
			t.Fatalf("Extract(%q): bad TS term %q", in, ts)
		}
	}
	for _, l := range got.Like {
		if utf8.RuneCountInString(l) < minTermRunes || strings.ContainsAny(l, "%\\' \t\n") {
			t.Fatalf("Extract(%q): bad Like term %q", in, l)
		}
	}
	q := TSQuery(got.TS)
	if !tsqueryGrammar.MatchString(q) {
		t.Fatalf("Extract(%q): TSQuery = %q violates grammar", in, q)
	}
	if (q == "") != (len(got.TS) == 0) {
		t.Fatalf("Extract(%q): TSQuery %q vs TS %q", in, q, got.TS)
	}
}

// FuzzTSQuery 는 어떤 입력에도 (1) Extract 결과가 불변식을 지키고,
// (2) Extract 를 거치지 않은 임의의 항목 목록을 TSQuery 에 바로 넣어도 출력이
// 문법을 벗어나지 않는지 본다.
func FuzzTSQuery(f *testing.F) {
	for _, seed := range []string{
		"이번 주 회의 일정 알려줘",
		"'); DROP TABLE documents; --",
		`a & b | !c <-> d:* 'e' \f`,
		"foo@bar.com 010-1234-5678 v0.24.0",
		"😀🎉",
		"İstanbul ǅemal ﬁle",
		"회의' | '일정",
		strings.Repeat("가나다라 ", 300),
		"회의",
		"a\x00b\xff회의",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		checkTermsInvariants(t, in, Extract(in))
		raw := TSQuery(strings.Fields(in))
		if !tsqueryGrammar.MatchString(raw) {
			t.Fatalf("TSQuery(Fields(%q)) = %q violates grammar", in, raw)
		}
	})
}
