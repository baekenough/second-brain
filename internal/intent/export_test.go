// export_test.go exposes internal LLMClassifier fields for black-box tests
// in the intent_test package (mirrors internal/collector/export_test.go's
// convention). Compiled only during testing.
package intent

import (
	"regexp"
	"time"
)

// SetNow overrides the clock LLMClassifier uses for relative date-phrase
// resolution ("지난달", "오늘", ...), giving tests a fixed, deterministic
// reference time instead of the real time.Now.
func SetNow(c *LLMClassifier, now func() time.Time) {
	c.now = now
}

// SetPlannerNow overrides the clock LLMPlanner uses to resolve relative date
// phrases and to tell the model what "today" is, mirroring SetNow's role for
// LLMClassifier.
func SetPlannerNow(p *LLMPlanner, now func() time.Time) {
	p.now = now
}

// DisableDeterministicPath forces every Plan call onto the LLM path. It exists
// so the golden set (design spec §8) can be run against both paths with one
// table: the regex layer is a cache for common phrasings, not a second set of
// semantics, and a table that only ever exercised the cache could not detect
// the two diverging (spec §12 R6).
func DisableDeterministicPath(p *LLMPlanner) {
	p.deterministicDisabled = true
}

// TimePhraseRegexes 는 DeterministicWindow 가 기간으로 해석하는 모든 정규식이다.
// internal/sparseq 의 시간 표현 목록이 이것을 전부 덮는지 검사하는
// 커버리지 테스트(sparseq_coverage_test.go)가 쓴다 — 여기에 새 정규식을
// 추가하면 그 목록에도 넣어야 테스트가 통과한다.
func TimePhraseRegexes() []*regexp.Regexp {
	out := append([]*regexp.Regexp(nil), periodMentionRegexes...)
	return append(out,
		exactDayRe, bareMonthRe,
		daysAgoRe, weeksAgoRe, monthsAgoRe,
		lastNDaysRe, lastNWeeksRe,
		weekdayInWeekRe, bareWeekdayRe,
	)
}
