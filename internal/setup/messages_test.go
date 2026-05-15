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

// TestMsgFallback verifies the three-tier fallback: current lang → English → key.
func TestMsgFallback(t *testing.T) {
	orig := lang
	defer func() { lang = orig }()

	cases := []struct {
		name    string
		setLang string
		key     string
		want    string
	}{
		{
			name:    "existing key in English",
			setLang: "en",
			key:     "welcome",
			want:    messages["en"]["welcome"],
		},
		{
			name:    "key only in ko map falls back to English",
			setLang: "en",
			// Temporarily seed a ko-only key in the test; we use a real en key
			// to verify the en-fallback path instead, as mutating messages is racy.
			// This case is covered by TestMsg_FallsBackToEnglish above.
			// Here we verify that a *missing* key returns the key itself.
			key:  "__nonexistent_key__",
			want: "__nonexistent_key__",
		},
		{
			name:    "key missing entirely returns key",
			setLang: "zz",
			key:     "__totally_missing__",
			want:    "__totally_missing__",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			lang = tc.setLang
			got := msg(tc.key)
			if got != tc.want {
				t.Errorf("msg(%q) with lang=%q = %q, want %q", tc.key, tc.setLang, got, tc.want)
			}
		})
	}
}

// TestDetectLang_AdditionalCases covers lowercase POSIX and ko.UTF-8 input.
func TestDetectLang_AdditionalCases(t *testing.T) {
	cases := []struct {
		langEnv string
		want    string
	}{
		{"ko.UTF-8", "ko"},   // bare ko with encoding suffix, no territory
		{"posix", "en"},      // lowercase POSIX — verifies strings.ToLower path
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.langEnv, func(t *testing.T) {
			t.Setenv("LANG", tc.langEnv)
			t.Setenv("LANGUAGE", "")

			got := detectLang()
			if got != tc.want {
				t.Errorf("detectLang() with LANG=%q = %q, want %q", tc.langEnv, got, tc.want)
			}
		})
	}
}
