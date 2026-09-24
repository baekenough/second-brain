package api

import (
	"time"

	"github.com/baekenough/second-brain/internal/intent"
)

// WithClock injects a deterministic clock into s AND, when the currently
// wired intentClassifier/queryPlanner are the concrete LLM-backed types this
// package constructs by default (NewServer, s.planner's fallback), into
// those too.
//
// This exists for internal/askeval (issue #266): an offline evaluation run
// needs EVERY "current time" a fixture's as_of value touches — the Stage 3
// system prompt (s.now, buildAskSystemPrompt), the deterministic window
// regexes (intent.LLMPlanner.deterministicPlan), and the LLM
// date-phrase-detection fallback (intent.LLMClassifier) — to agree on the
// SAME instant, or a fixture asserting "어제" resolves to one window in the
// prompt and a different one in retrieval would fail for a reason that has
// nothing to do with the code under test. Three separate unexported `now`
// fields exist (deep-plan #268 finding F8) precisely so each package's tests
// can pin its own clock independently; this builder is the one place that
// pins all three at once from OUTSIDE package api, which no existing
// same-package `srv.now = ...` test helper needed to do.
//
// A type assertion, not a stronger requirement on the Server/Classifier/
// Planner fields, is used for the classifier/planner half: both fields are
// interfaces (intent.Classifier, intent.Planner) precisely so a caller can
// substitute a test fake that has no clock at all (ask_test.go's
// fakeIntentClassifier). Silently skipping a fake that doesn't implement
// the WithNow-returning shape is the correct behavior here, not an error —
// see intent.LLMClassifier.WithNow / intent.LLMPlanner.WithNow for the
// exported setters this asserts against.
//
// 주의: WithClock은 고루틴 안전하지 않다 — s.now와 두 필드를 잠금 없이 그대로
// 대입한다. 서버가 이미 요청을 처리하기 시작한 뒤 호출하면 데이터 레이스가
// 된다. eval/test 전용 빌더이므로, 서버 시작 전(요청 처리 이전)에만 호출할 것.
func (s *Server) WithClock(now func() time.Time) *Server {
	s.now = now
	if c, ok := s.intentClassifier.(*intent.LLMClassifier); ok {
		c.WithNow(now)
	}
	if p, ok := s.queryPlanner.(*intent.LLMPlanner); ok {
		p.WithNow(now)
	}
	return s
}
