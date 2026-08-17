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
