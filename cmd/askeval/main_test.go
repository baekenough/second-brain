package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoFixturesDir locates eval/ask/fixtures relative to this test file, so
// the test works regardless of the working directory `go test` is invoked
// from.
func repoFixturesDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// cmd/askeval/main_test.go -> repo root -> eval/ask/fixtures
	return filepath.Join(filepath.Dir(file), "..", "..", "eval", "ask", "fixtures")
}

// TestRun_JudgeShadowRemoteRefusesWithoutAPIKey asserts --judge=shadow
// --judge-backend=remote exits 2 (a configuration error, never a pipeline
// finding) when ASKEVAL_JUDGE_API_KEY is unset — issue #273's "기본값은
// 끈다" requirement holding at the CLI's own entry point, not just inside
// askeval.NewRemoteClaimJudge's own unit tests. No real API call is made:
// the guard fires before any client is constructed.
func TestRun_JudgeShadowRemoteRefusesWithoutAPIKey(t *testing.T) {
	t.Setenv("ASKEVAL_JUDGE_API_KEY", "")
	code := run([]string{"--fixtures", repoFixturesDir(t), "--judge", "shadow", "--judge-backend", "remote"})
	if code != 2 {
		t.Errorf("want exit code 2, got %d", code)
	}
}

// TestRun_JudgeBackendRejectsUnknownValue asserts an unrecognized
// --judge-backend value is caught as a usage error (exit 2) before any
// fixture is even loaded.
func TestRun_JudgeBackendRejectsUnknownValue(t *testing.T) {
	code := run([]string{"--fixtures", repoFixturesDir(t), "--judge-backend", "bogus"})
	if code != 2 {
		t.Errorf("want exit code 2, got %d", code)
	}
}

// TestRun_DefaultJudgeOff asserts the default run (--judge=off, implicit)
// exits 0 over the real committed fixture set and writes a report whose
// judge_mode is "off" with no judge_backend recorded.
func TestRun_DefaultJudgeOff(t *testing.T) {
	out := filepath.Join(t.TempDir(), "report.json")
	code := run([]string{"--fixtures", repoFixturesDir(t), "--out", out})
	if code != 0 {
		t.Fatalf("want exit code 0, got %d", code)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(string(b), `"judge_mode": "off"`) {
		t.Errorf("report does not show judge_mode=off:\n%s", b)
	}
	if strings.Contains(string(b), `"judge_backend"`) {
		t.Errorf("report unexpectedly records judge_backend when judge_mode=off:\n%s", b)
	}
}

// TestRun_JudgeShadowFakeSucceeds asserts --judge=shadow (default
// --judge-backend=fake) exits 0 over the real committed fixture set and
// records judge_backend="fake" in the report.
func TestRun_JudgeShadowFakeSucceeds(t *testing.T) {
	out := filepath.Join(t.TempDir(), "report.json")
	code := run([]string{"--fixtures", repoFixturesDir(t), "--judge", "shadow", "--out", out})
	if code != 0 {
		t.Fatalf("want exit code 0, got %d", code)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(string(b), `"judge_backend": "fake"`) {
		t.Errorf("report does not show judge_backend=fake:\n%s", b)
	}
}
