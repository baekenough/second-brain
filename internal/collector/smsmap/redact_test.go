package smsmap_test

import (
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
