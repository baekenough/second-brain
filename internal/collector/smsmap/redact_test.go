package smsmap_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/collector/smsmap"
)

// TestRedactPII covers the structured-PII patterns introduced for issue #163:
// Korean resident registration numbers (RRN), Korean phone numbers, OTP/auth
// codes, and bank-account-like hyphenated digit runs — plus negative cases
// (dates, timestamps, short codes without auth context) that must survive
// untouched.
func TestRedactPII(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantRedact bool // whether PIIRedactionToken must appear in the output
	}{
		// --- positive: RRN ---
		{
			name:       "plausible RRN is redacted",
			input:      "제 주민번호는 900101-1234567 입니다",
			wantRedact: true,
		},
		{
			name:       "RRN with implausible month is not redacted",
			input:      "번호 202607-1234567 확인",
			wantRedact: false,
		},
		{
			name:       "RRN with implausible century digit is not redacted",
			input:      "번호 900101-9234567 확인", // gender digit '9' out of [1-8]
			wantRedact: false,
		},

		// --- positive: Korean phone numbers ---
		{
			name:       "mobile phone with hyphens is redacted",
			input:      "제 번호는 010-1234-5678 입니다",
			wantRedact: true,
		},
		{
			name:       "mobile phone without hyphens is redacted",
			input:      "번호 01012345678 로 연락주세요",
			wantRedact: true,
		},
		{
			name:       "landline phone is redacted",
			input:      "회사 전화는 02-1234-5678 입니다",
			wantRedact: true,
		},

		// --- positive: Korean phone numbers, international (+82) form (issue #167) ---
		{
			name:       "+82 mobile with hyphens is redacted",
			input:      "제 번호는 +82 10-1234-5678 입니다",
			wantRedact: true,
		},
		{
			name:       "+82 mobile with all-hyphen separators is redacted",
			input:      "제 번호는 +82-10-1234-5678 입니다",
			wantRedact: true,
		},
		{
			name:       "+82 mobile with parenthesised trunk zero is redacted",
			input:      "제 번호는 +82 (0)10-1234-5678 입니다",
			wantRedact: true,
		},
		{
			name:       "+82 Seoul landline is redacted",
			input:      "회사 번호는 +82 2-1234-5678 입니다",
			wantRedact: true,
		},
		{
			name:       "+82 regional landline is redacted",
			input:      "회사 번호는 +82 31-1234-5678 입니다",
			wantRedact: true,
		},
		{
			name:       "0082 with space separators is redacted",
			input:      "국제전화 0082 10 1234 5678 로 연락주세요",
			wantRedact: true,
		},
		{
			name:       "bare 82 country code with unbroken digits is redacted",
			input:      "국제전화 8210 12345678 로 연락주세요",
			wantRedact: true,
		},

		// --- positive: bank account ---
		{
			name:       "hyphenated bank account is redacted",
			input:      "계좌번호 110-123-4567890 로 입금해주세요",
			wantRedact: true,
		},

		// --- positive: OTP near auth keyword ---
		{
			name:       "digits near auth keyword are redacted",
			input:      "인증번호는 123456 입니다",
			wantRedact: true,
		},
		{
			name:       "digits near verification keyword are redacted",
			input:      "Your verification code is 987654",
			wantRedact: true,
		},

		// --- negative: dates / timestamps / short codes ---
		{
			name:       "ISO date is not redacted",
			input:      "오늘은 2026-07-13 입니다",
			wantRedact: false,
		},
		{
			name:       "unbroken numeric timestamp is not redacted",
			input:      "파일명은 20260713153000 입니다",
			wantRedact: false,
		},
		{
			name:       "short digit run without auth context is not redacted",
			input:      "발주 수량은 1234 개 입니다",
			wantRedact: false,
		},
		{
			name: "digit run far outside the auth-keyword proximity window is not redacted",
			input: "인증번호 확인 부탁드립니다. " +
				strings.Repeat("filler text with no digits here. ", 10) +
				"발주 수량은 123456 개 입니다.",
			wantRedact: false,
		},
		{
			name:       "plain conversational text is not redacted",
			input:      "안녕하세요 오늘 회의는 3시에 시작합니다",
			wantRedact: false,
		},

		// --- negative: issue #167 country-code false-positive guards ---
		{
			name:       "standalone 82 as an age is not redacted",
			input:      "제 나이는 82 입니다",
			wantRedact: false,
		},
		{
			name:       "standalone 82 as a score is not redacted",
			input:      "시험 점수는 82점 입니다",
			wantRedact: false,
		},
		{
			name:       "US +1 number is not swallowed by the 82 country-code pattern",
			input:      "미국 번호는 +1 202 555 0123 입니다",
			wantRedact: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := smsmap.RedactPII(tc.input)
			contains := strings.Contains(got, smsmap.PIIRedactionToken)

			if contains != tc.wantRedact {
				t.Errorf("RedactPII(%q) = %q; contains %q = %v, want %v",
					tc.input, got, smsmap.PIIRedactionToken, contains, tc.wantRedact)
			}
			if !tc.wantRedact && got != tc.input {
				t.Errorf("RedactPII(%q) = %q, want unchanged (negative case)", tc.input, got)
			}
		})
	}
}

// trailingDigitGroupRe detects the leak pattern this regression guards
// against: a redaction token immediately followed by a (possibly
// hyphen-prefixed) digit group, i.e. the tail end of a phone number that
// escaped full redaction because it was only partially consumed by
// bankAccountRe (see TestRedactPII_BareCountryCodeWithSeparatorFullyRedacted).
var trailingDigitGroupRe = regexp.MustCompile(`\[REDACTED\]-?\d`)

// TestRedactPII_BareCountryCodeWithSeparatorFullyRedacted covers the
// separator-delimited bare "82" country-code form (no leading '+' or "0082"
// exit code), which previously fell through every koreanPhoneIntl* pattern
// (none of which allow a separator between "82" and the area code) and was
// then only PARTIALLY consumed by bankAccountRe, leaking the final digit
// group (e.g. "82-10-1234-5678" -> "[REDACTED]-5678"). The whole number must
// now be redacted in one pass with no leftover digits.
func TestRedactPII_BareCountryCodeWithSeparatorFullyRedacted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{name: "hyphen separators", input: "82-10-1234-5678"},
		{name: "space separators", input: "82 10 1234 5678"},
		{name: "dot separators", input: "82.10.1234.5678"},
		{name: "hyphen separators in sentence", input: "국제전화 82-10-1234-5678 로 연락주세요"},
		{name: "space separators in sentence", input: "국제전화 82 10 1234 5678 로 연락주세요"},
		{name: "dot separators in sentence", input: "국제전화 82.10.1234.5678 로 연락주세요"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := smsmap.RedactPII(tc.input)

			if !strings.Contains(got, smsmap.PIIRedactionToken) {
				t.Fatalf("RedactPII(%q) = %q, want redaction to occur", tc.input, got)
			}
			if trailingDigitGroupRe.MatchString(got) {
				t.Errorf("RedactPII(%q) = %q, leaked a trailing digit group after the redaction token", tc.input, got)
			}
			if strings.ContainsAny(got, "0123456789") {
				t.Errorf("RedactPII(%q) = %q, want no digits left over from the phone number", tc.input, got)
			}
		})
	}
}
