// export_test.go exposes internal LLMClassifier fields for black-box tests
// in the intent_test package (mirrors internal/collector/export_test.go's
// convention). Compiled only during testing.
package intent

import "time"

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
