package intent

import "time"

// WithNow injects a deterministic clock into c, mirroring the unexported
// now field's existing "nil means time.Now" convention (see c.nowFunc).
// Exported so a caller OUTSIDE this package (internal/api's WithClock
// builder, internal/askeval's offline runner) can pin "today" without
// reaching into an unexported field from a different package — the
// existing _test.go convention of `c.now = ...` only works for same-package
// tests. Returns c so it composes with the existing constructor chain
// (NewLLMClassifier(...).WithNow(...)).
//
// 주의: WithNow는 고루틴 안전하지 않다(잠금 없이 c.now를 대입) — 서버가 요청
// 처리를 시작하기 전(eval/test 초기화 시점)에만 호출할 것.
func (c *LLMClassifier) WithNow(now func() time.Time) *LLMClassifier {
	c.now = now
	return c
}

// WithNow injects a deterministic clock into p, the Planner counterpart of
// LLMClassifier.WithNow above — see that method's doc comment for why this
// exists as an exported builder rather than direct field access.
//
// 주의: WithNow는 고루틴 안전하지 않다(잠금 없이 p.now를 대입) — 서버가 요청
// 처리를 시작하기 전(eval/test 초기화 시점)에만 호출할 것.
func (p *LLMPlanner) WithNow(now func() time.Time) *LLMPlanner {
	p.now = now
	return p
}
