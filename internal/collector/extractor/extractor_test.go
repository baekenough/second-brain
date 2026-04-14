package extractor

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeText(t *testing.T) {
	tests := []struct {
		name  string
		input string
		check func(t *testing.T, got string)
	}{
		{
			name:  "empty string",
			input: "",
			check: func(t *testing.T, got string) {
				if got != "" {
					t.Errorf("expected empty, got %q", got)
				}
			},
		},
		{
			name:  "no NUL bytes, valid UTF-8",
			input: "Hello, World!",
			check: func(t *testing.T, got string) {
				if got != "Hello, World!" {
					t.Errorf("expected unchanged, got %q", got)
				}
			},
		},
		{
			name:  "NUL byte removed (Postgres SQLSTATE 22021 trigger)",
			input: "hello\x00world",
			check: func(t *testing.T, got string) {
				if strings.Contains(got, "\x00") {
					t.Error("NUL byte must be removed")
				}
				if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
					t.Errorf("surrounding text must be preserved, got %q", got)
				}
			},
		},
		{
			name:  "multiple NUL bytes",
			input: "\x00\x00text\x00\x00",
			check: func(t *testing.T, got string) {
				if strings.Contains(got, "\x00") {
					t.Error("all NUL bytes must be removed")
				}
			},
		},
		{
			name:  "invalid UTF-8 sequence replaced",
			input: "valid\xff\xfeinvalid",
			check: func(t *testing.T, got string) {
				if !utf8.ValidString(got) {
					t.Error("output must be valid UTF-8")
				}
			},
		},
		{
			name:  "excessive blank lines collapsed",
			input: "a\n\n\n\n\nb",
			check: func(t *testing.T, got string) {
				if strings.Contains(got, "\n\n\n") {
					t.Errorf("3+ consecutive newlines must be collapsed, got %q", got)
				}
				if !strings.Contains(got, "a") || !strings.Contains(got, "b") {
					t.Errorf("content must be preserved, got %q", got)
				}
			},
		},
		{
			name:  "Korean UTF-8 text unchanged",
			input: "안녕하세요 세계",
			check: func(t *testing.T, got string) {
				if got != "안녕하세요 세계" {
					t.Errorf("valid UTF-8 text must pass through unchanged, got %q", got)
				}
			},
		},
		{
			name:  "NUL and invalid UTF-8 combined",
			input: "pdf\x00output\xff\xfewith issues",
			check: func(t *testing.T, got string) {
				if strings.Contains(got, "\x00") {
					t.Error("NUL byte must be removed")
				}
				if !utf8.ValidString(got) {
					t.Error("output must be valid UTF-8")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeText(tc.input)
			tc.check(t, got)
		})
	}
}

func TestTruncateUTF8(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxBytes int
		wantTail string
	}{
		{
			name:     "no truncation needed",
			input:    "short",
			maxBytes: 100,
			wantTail: "",
		},
		{
			name:     "truncation adds suffix",
			input:    strings.Repeat("a", 200),
			maxBytes: 100,
			wantTail: "\n[content truncated]",
		},
		{
			name:     "truncation preserves valid UTF-8 boundary",
			input:    strings.Repeat("안", 100), // 3 bytes each
			maxBytes: 10,
			wantTail: "\n[content truncated]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateUTF8(tc.input, tc.maxBytes)
			if !utf8.ValidString(got) {
				t.Error("TruncateUTF8 must return valid UTF-8")
			}
			if tc.wantTail != "" && !strings.HasSuffix(got, tc.wantTail) {
				t.Errorf("expected suffix %q, got %q", tc.wantTail, got)
			}
			if tc.wantTail == "" && len(got) != len(tc.input) {
				t.Errorf("expected no truncation: len=%d, got len=%d", len(tc.input), len(got))
			}
		})
	}
}
