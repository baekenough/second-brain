package smsmap

import (
	"regexp"
	"strconv"
	"strings"
)

// PIIRedactionToken is the placeholder inserted in place of detected
// structured PII. It matches the existing SMS OTP-redaction convention (see
// otpDigitsRe usage in MapSMS) so every source (SMS, call transcripts)
// surfaces the same redaction marker downstream.
const PIIRedactionToken = "[REDACTED]"

// authContextWindow is the number of bytes examined on either side of a
// candidate OTP/auth digit run when deciding whether it sits in an
// authentication context (e.g. near "인증번호"). MapSMS gates redaction on the
// ENTIRE SMS body matching authLikeRe because SMS bodies are short; call
// transcripts are far longer free-form text, so gating on the whole document
// would either redact unrelated numbers throughout a long call or (if we
// required the phrase to dominate the transcript) miss legitimate auth
// exchanges buried in a long conversation. Scoping the check to a local
// window around each digit run keeps the SMS heuristic's intent — "digits
// near an auth phrase are OTPs" — while bounding the blast radius.
//
// The window is a heuristic, not a hard guarantee: a byte-offset window can
// occasionally slice a multi-byte UTF-8 rune at its edges when computing the
// substring bounds. That only risks the window's edge character contributing
// a partial rune to the match attempt; regexp.MatchString does not panic on
// invalid UTF-8, it simply may fail to match right at the cut boundary. Given
// this is a proximity heuristic (not the redaction boundary itself — the
// otpDigitsRe match position, which stays rune-safe via FindAllStringIndex,
// is what actually gets redacted) this tradeoff is acceptable.
const authContextWindow = 40

var (
	// rrnRe matches candidate Korean resident registration numbers: 6 digits
	// (YYMMDD) + hyphen + 7 digits (century/gender digit + serial + checksum).
	// Matches are passed through redactRRNIfPlausible, which rejects
	// obviously-invalid dates/century-digits so that generic hyphenated
	// 6-7 digit sequences (which are NOT necessarily RRNs) are not blindly
	// redacted.
	rrnRe = regexp.MustCompile(`\b\d{6}-\d{7}\b`)

	// koreanPhoneRe matches Korean mobile numbers (010/011/016/017/018/019)
	// and landline numbers (02 Seoul, or 031-065 style 3-digit area codes),
	// with or without hyphen/space separators between groups.
	//
	// Scope (issue #163): only DIGIT-FORM phone numbers are covered.
	// Spoken-form numbers (e.g. "공일공에 일이삼사에 오육칠팔") require a
	// number-word grammar that is out of scope for this regex-only pass and
	// is deferred to a follow-up.
	koreanPhoneRe = regexp.MustCompile(`\b(01[016789]|02|0[3-6][1-5])[-. ]?\d{3,4}[-. ]?\d{4}\b`)

	// koreanAreaCodeRe is the shared "area code without its domestic trunk
	// zero" alternation reused by all three country-code phone patterns
	// below: mobile 10/11/16/17/18/19, Seoul landline 2, or regional landline
	// 31-65 (2-digit, first digit 3-6 / second digit 1-5 — mirrors
	// koreanPhoneRe's 0[3-6][1-5] minus the leading trunk zero).
	koreanAreaCodeRe = `(?:1[016789]|2|[3-6][1-5])`

	// koreanPhoneIntlPlusRe matches Korean phone numbers (issue #167) written
	// with an explicit "+82" international dialing prefix, e.g.
	// "+82 10-1234-5678", "+82-10-1234-5678", "+82 (0)10-1234-5678",
	// "+82 2-1234-5678" (Seoul landline), "+82 31-1234-5678" (regional
	// landline). The optional "(0)" covers the common convention of showing
	// the domestic trunk zero in parentheses even though it is dropped when
	// actually dialing with the country code.
	//
	// Deliberately NO leading \b: RE2 (Go's regexp engine, used here) has no
	// lookaround support, and \b only matches at a transition between a word
	// char and a non-word char. '+' is itself a non-word character, so a
	// leading \b would fail to match the overwhelmingly common case of "+82"
	// preceded by whitespace (space is also non-word — no transition, no
	// boundary). "+82" is an unambiguous enough marker on its own that
	// omitting the leading assertion does not introduce meaningful
	// false-positive risk; the trailing \b (after a digit, a word char)
	// still works normally.
	koreanPhoneIntlPlusRe = regexp.MustCompile(`\+82[-. ]?\(?0?\)?[-. ]?` + koreanAreaCodeRe + `[-. ]?\d{3,4}[-. ]?\d{4}\b`)

	// koreanPhoneIntl00Re matches the same numbers written with the "0082"
	// international exit-code prefix, e.g. "0082 10 1234 5678". Digit-led,
	// so a leading \b correctly requires it NOT be preceded by another digit
	// (avoiding a partial match inside a longer numeric run).
	koreanPhoneIntl00Re = regexp.MustCompile(`\b0082[-. ]?\(?0?\)?[-. ]?` + koreanAreaCodeRe + `[-. ]?\d{3,4}[-. ]?\d{4}\b`)

	// koreanPhoneIntlBareRe matches the bare "82" country-code form with
	// neither a '+' nor a "00" exit-code marker, covering both the unbroken
	// digit-run shape (e.g. "8210 12345678") and the fully-separated shape
	// (e.g. "82-10-1234-5678", "82 10 1234 5678", "82.10.1234.5678").
	//
	// A single optional [-. ] separator is allowed between "82" and the area
	// code — mirroring the separator already allowed between the area code
	// and the following digit groups — so that a hyphen/space/dot-delimited
	// bare-82 number is matched and redacted in ONE pass, rather than being
	// partially consumed by the (later-run) bankAccountRe pattern and
	// leaving a trailing digit group unredacted (e.g. "82-10-1234-5678"
	// previously left "-5678" leaking as "[REDACTED]-5678").
	//
	// This remains conservative against Finding-3-style false positives: the
	// area code segment must still match one of the EXACT
	// mobile/Seoul/regional patterns in koreanAreaCodeRe, and the trailing
	// two digit groups (\d{3,4} then \d{4}) must still both be present. A
	// coincidental standalone "82" (e.g. an age "82" or a score "82점") is
	// not followed by a valid area-code-shaped digit group at all, so it
	// still never matches — allowing the separator here does not by itself
	// make "82 <anything>" match; the full digit-group shape is still
	// required.
	koreanPhoneIntlBareRe = regexp.MustCompile(`\b82[-. ]?` + koreanAreaCodeRe + `[-. ]?\d{3,4}[-. ]?\d{4}\b`)

	// bankAccountRe matches hyphen-delimited digit sequences shaped like a
	// Korean bank account number (bank/branch code + segment + serial,
	// commonly displayed as 3 hyphenated groups, e.g. "110-123-456789").
	// Requiring exactly two hyphens (three groups) is what keeps this from
	// matching ISO dates (YYYY-MM-DD is 4-2-2 digits — the first group here
	// is capped at 3 digits) or plain unbroken numeric timestamps (which have
	// no hyphens at all). Unhyphenated long digit runs are intentionally NOT
	// treated as bank-account-like: without hyphens they are indistinguishable
	// from timestamps/IDs and are not "safely detectable".
	bankAccountRe = regexp.MustCompile(`\b\d{2,3}-\d{2,6}-\d{4,8}\b`)

	// otpDigitsRe (defined in smsmap.go) matches the 4-8 digit runs that
	// MapSMS redacts unconditionally once an SMS body is judged auth-like.
	// RedactPII reuses the SAME digit-run pattern for call transcripts, but
	// (unlike MapSMS) only redacts a given run when an auth keyword appears
	// within authContextWindow bytes of it — see redactAuthContextDigits.

	// authKeywordRe matches only the KEYWORD phrases used by MapSMS's
	// authLikeRe (Korean/English authentication phrases), deliberately
	// excluding authLikeRe's bare `\b\d{4,8}\b` alternative. RedactPII needs a
	// keyword-only signal to test PROXIMITY to a candidate digit run; reusing
	// authLikeRe as-is would make every digit run its own auth "context"
	// (trivially always redacting).
	authKeywordRe = regexp.MustCompile(`(?i)인증번호|본인.{0,2}확인|verification|타인에게|otp`)
)

// RedactPII replaces structured, regex-detectable PII in s with
// PIIRedactionToken:
//   - Korean resident registration numbers (RRN): \d{6}-\d{7}, plausibility-
//     checked (see redactRRNIfPlausible).
//   - Korean phone numbers, domestic trunk-prefix form (see koreanPhoneRe)
//     and international country-code form — with/without "+"/"0082", and
//     with hyphen/space/dot separators (see koreanPhoneIntlPlusRe,
//     koreanPhoneIntl00Re, koreanPhoneIntlBareRe).
//   - Bank-account-like hyphenated digit runs (see bankAccountRe).
//   - OTP/auth codes: 4-8 digit runs (the same pattern MapSMS uses) that
//     appear near an authentication keyword (see redactAuthContextDigits).
//
// Scope (issue #163, this release): regex-based structured PII only.
// Spoken-form phone numbers (e.g. "공일공에…") and NER-based name redaction
// are explicitly OUT of scope and deferred to a follow-up — both would
// require either a large hand-written grammar or a model/NER dependency,
// neither of which this pass introduces.
//
// Scope (issue #167): only Korean (+82) country-code digit-form numbers are
// covered. Other countries' international numbers (e.g. "+1-...") are out
// of scope and are left untouched by these patterns — they simply don't
// match the literal "82"/"+82"/"0082" markers required here.
//
// Order matters: RRN and phone patterns run before the bank-account pattern
// so that, e.g., a phone number's "010-1234-5678" hyphenated groups are
// already replaced with PIIRedactionToken (no longer digits) by the time the
// (structurally similar, 3-hyphenated-group) bank-account pattern runs,
// avoiding a confusing double-match. The OTP/auth pass runs last since it
// operates on whatever raw digit runs remain.
func RedactPII(s string) string {
	s = rrnRe.ReplaceAllStringFunc(s, redactRRNIfPlausible)
	s = koreanPhoneRe.ReplaceAllString(s, PIIRedactionToken)
	s = koreanPhoneIntlPlusRe.ReplaceAllString(s, PIIRedactionToken)
	s = koreanPhoneIntl00Re.ReplaceAllString(s, PIIRedactionToken)
	s = koreanPhoneIntlBareRe.ReplaceAllString(s, PIIRedactionToken)
	s = bankAccountRe.ReplaceAllString(s, PIIRedactionToken)
	s = redactAuthContextDigits(s)
	return s
}

// redactRRNIfPlausible is the rrnRe.ReplaceAllStringFunc callback. match is
// always exactly "DDDDDD-DDDDDDD" (rrnRe guarantees the shape). The digit
// sequence is only redacted when it looks like a plausible RRN:
//   - the first 6 digits parse as a YYMMDD date (month 1-12, day 1-31), and
//   - the digit immediately after the hyphen (century/gender code) is 1-8.
//
// This rejects generic 6-7 digit hyphenated sequences (e.g. arbitrary
// reference numbers) that happen to share the RRN shape but are not
// themselves a date, so we do not over-redact non-PII content.
func redactRRNIfPlausible(match string) string {
	// match = 6 digits, '-', 7 digits (guaranteed by rrnRe).
	mm, mmErr := strconv.Atoi(match[2:4])
	dd, ddErr := strconv.Atoi(match[4:6])
	if mmErr != nil || ddErr != nil || mm < 1 || mm > 12 || dd < 1 || dd > 31 {
		return match // not a plausible date — leave unredacted
	}
	genderDigit := match[7]
	if genderDigit < '1' || genderDigit > '8' {
		return match // not a valid century/gender code — leave unredacted
	}
	return PIIRedactionToken
}

// redactAuthContextDigits redacts otpDigitsRe (4-8 digit run) matches that
// appear within authContextWindow bytes of an authKeywordRe match, leaving
// digit runs elsewhere in s untouched. See the authContextWindow doc comment
// for the rationale and its known heuristic edge-cases.
func redactAuthContextDigits(s string) string {
	locs := otpDigitsRe.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s
	}

	var b strings.Builder
	last := 0
	for _, loc := range locs {
		start, end := loc[0], loc[1]

		winStart := start - authContextWindow
		if winStart < 0 {
			winStart = 0
		}
		winEnd := end + authContextWindow
		if winEnd > len(s) {
			winEnd = len(s)
		}

		if !authKeywordRe.MatchString(s[winStart:winEnd]) {
			continue // no auth keyword nearby — not an OTP, leave this run alone
		}

		b.WriteString(s[last:start])
		b.WriteString(PIIRedactionToken)
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}
