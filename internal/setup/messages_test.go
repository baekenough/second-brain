package setup

import (
	"testing"
)

// TestDetectLang exercises detectLang() with a range of LANG values.
// t.Setenv cannot be combined with t.Parallel — subtests run sequentially.
func TestDetectLang(t *testing.T) {
	cases := []struct {
		langEnv string
		want    string
	}{
		{"ko_KR.UTF-8", "ko"},
		{"ko_KR", "ko"},
		{"ko", "ko"},
		{"en_US.UTF-8", "en"},
		{"en_US", "en"},
		{"en", "en"},
		{"C", "en"},
		{"POSIX", "en"},
		{"", "en"},
		{"fr_FR.UTF-8", "en"}, // unsupported → fallback
		{"ja_JP.UTF-8", "en"}, // unsupported → fallback
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.langEnv, func(t *testing.T) {
			t.Setenv("LANG", tc.langEnv)
			t.Setenv("LANGUAGE", "") // clear LANGUAGE to avoid interference

			got := detectLang()
			if got != tc.want {
				t.Errorf("detectLang() with LANG=%q = %q, want %q", tc.langEnv, got, tc.want)
			}
		})
	}
}

func TestDetectLang_LanguageEnvPriority(t *testing.T) {
	// LANGUAGE is checked before LANG. When LANG is empty but LANGUAGE is set,
	// it should take precedence.
	t.Setenv("LANG", "")
	t.Setenv("LANGUAGE", "ko_KR.UTF-8")
	got := detectLang()
	if got != "ko" {
		t.Errorf("detectLang() with LANGUAGE=ko_KR.UTF-8 = %q, want %q", got, "ko")
	}
}

func TestDetectLang_ColonSeparatedLanguage(t *testing.T) {
	// LANGUAGE may be a colon-separated priority list; first entry wins.
	t.Setenv("LANG", "")
	t.Setenv("LANGUAGE", "ko_KR:en_US")
	got := detectLang()
	if got != "ko" {
		t.Errorf("detectLang() with LANGUAGE=ko_KR:en_US = %q, want ko", got)
	}
}

func TestMsg_FallsBackToEnglish(t *testing.T) {
	// Temporarily override global lang.
	orig := lang
	lang = "zz" // unsupported language
	defer func() { lang = orig }()

	got := msg("welcome")
	want := messages["en"]["welcome"]
	if got != want {
		t.Errorf("msg(\"welcome\") with lang=zz = %q, want %q", got, want)
	}
}

func TestMsg_KoreanMessages(t *testing.T) {
	orig := lang
	lang = "ko"
	defer func() { lang = orig }()

	got := msg("welcome")
	want := messages["ko"]["welcome"]
	if got != want {
		t.Errorf("msg(\"welcome\") with lang=ko = %q, want %q", got, want)
	}
}
