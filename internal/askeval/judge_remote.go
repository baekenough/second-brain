package askeval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/baekenough/second-brain/internal/llm"
)

// errRemoteJudgeRefused wraps every reason NewRemoteClaimJudge can refuse
// to start — cmd/askeval reports it and exits 2 (a configuration error,
// not a run-time finding): issue #273 requires the remote backend to stay
// off by default and never run against anything but this repo's synthetic
// fixtures.
var errRemoteJudgeRefused = errors.New("askeval: remote judge refused to start")

// RemoteJudgeConfig configures NewRemoteClaimJudge. Every field is read
// from the environment by cmd/askeval, never defaulted inside this
// package: a judge that could turn itself on from ambient environment
// state alone would violate issue #273's "기본값은 끈다" (default off)
// requirement the moment something exported the wrong variable.
type RemoteJudgeConfig struct {
	llm.Config
	// FixturesDir is the --fixtures path this run actually used. The
	// judge refuses to start unless it resolves under this repo's
	// eval/ask/fixtures tree (issue #273: "개인 데이터를 외부 judge에
	// 보내지 않는다. 합성 fixture만 쓴다") — a remote judge run must be
	// structurally incapable of seeing anything but synthetic fixture
	// content, not merely trusted not to.
	FixturesDir string
}

// NewRemoteClaimJudge builds a ClaimJudge backed by a real, remote,
// OpenAI-compatible chat completion endpoint (internal/llm.Client — never
// local inference, per this repo's no-local-inference policy: project
// memory `project_no_local_inference_policy.md`). It is NEVER constructed
// by go test or CI; the only caller is cmd/askeval, itself gated behind an
// explicit --judge-backend=remote flag. This constructor does not trust
// that gate alone — it independently re-checks every precondition:
//
//   - cfg.APIKey or cfg.AuthFile must be set (an authenticated token
//     source) — refuses with no key configured at all.
//   - cfg.FixturesDir must resolve under this repo's eval/ask/fixtures
//     tree — refuses a --fixtures path pointed anywhere else, so a remote
//     judge run cannot see real user data even by an operator's mistake.
//   - no DATABASE_URL or *_DATABASE_URL environment variable may be set
//     in this process — a remote judge process has no legitimate reason
//     to also hold a database credential; refusing to start when one is
//     present is a cheap, structural guard against that combination ever
//     reaching a real corpus.
func NewRemoteClaimJudge(cfg RemoteJudgeConfig) (ClaimJudge, error) {
	if cfg.APIKey == "" && cfg.AuthFile == "" {
		return nil, fmt.Errorf("%w: no API key or auth file configured (set ASKEVAL_JUDGE_API_KEY)", errRemoteJudgeRefused)
	}
	if err := requireFixturesUnderRepo(cfg.FixturesDir); err != nil {
		return nil, fmt.Errorf("%w: %v", errRemoteJudgeRefused, err)
	}
	if leaked := findDatabaseURLEnv(); leaked != "" {
		return nil, fmt.Errorf("%w: %s is set in this process's environment — a remote judge process must never also hold a database credential", errRemoteJudgeRefused, leaked)
	}
	client := llm.New(cfg.Config, nil)
	if !client.Enabled() {
		return nil, fmt.Errorf("%w: llm.Client is not fully configured (base URL/model/token) after construction", errRemoteJudgeRefused)
	}
	return &remoteJudge{client: client}, nil
}

// requireFixturesUnderRepo rejects any --fixtures path that does not
// resolve under an "eval/ask/fixtures" directory component — a coarse but
// structural guard (issue #273's "합성 fixture만 쓴다"), independent of
// cmd/askeval's own --fixtures default.
func requireFixturesUnderRepo(dir string) error {
	if dir == "" {
		return errors.New("fixtures dir is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolving fixtures dir %q: %w", dir, err)
	}
	if !strings.Contains(filepath.ToSlash(abs), "/eval/ask/fixtures") {
		return fmt.Errorf("fixtures dir %q does not resolve under eval/ask/fixtures", dir)
	}
	return nil
}

// findDatabaseURLEnv returns the name of the first DATABASE_URL or
// *_DATABASE_URL environment variable set in this process, or "" if none
// is set.
func findDatabaseURLEnv() string {
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		k := kv[:eq]
		if k == "DATABASE_URL" || strings.HasSuffix(k, "_DATABASE_URL") {
			return k
		}
	}
	return ""
}

// remoteJudgeSystemPrompt instructs the model to answer with exactly one
// of the three Verdict string values and nothing else — remoteJudge.Judge
// parses the response by exact (case-insensitive, trimmed) match against
// VerdictSupported/VerdictUnsupported; anything else is VerdictError.
const remoteJudgeSystemPrompt = `당신은 평가 채점자입니다. 아래 "주장"이 "근거" 텍스트에 의해 명시적으로 뒷받침되는지만 판정하세요.
반드시 정확히 다음 두 단어 중 하나만 출력하십시오: supported, unsupported.
그 외의 설명, 문장부호, 줄바꿈을 추가하지 마십시오.`

// RemoteJudgePromptHash returns a short, deterministic digest of
// remoteJudgeSystemPrompt — cmd/askeval records it in
// Provenance.JudgePromptHash so a report reader can tell whether a prompt
// change invalidates a previous remote-judge shadow run, the same role
// FixtureSetHash plays for the fixture set itself.
func RemoteJudgePromptHash() string {
	h := sha256.Sum256([]byte(remoteJudgeSystemPrompt))
	return hex.EncodeToString(h[:])[:16]
}

// remoteJudge is NEVER constructed by go test or CI (see
// NewRemoteClaimJudge's doc comment) — this repo's no-local-inference
// policy requires any real inference call to go through a remote API, and
// this is the only ClaimJudge implementation in this package that makes
// one. It does not implement fixtureAware: it answers purely from the
// (claim, excerpt) text RunShadowJudge passes it, with no other state.
type remoteJudge struct {
	client *llm.Client
}

func (j *remoteJudge) Judge(ctx context.Context, claim, excerpt string) (Verdict, error) {
	msg := fmt.Sprintf("주장: %s\n근거: %s", claim, excerpt)
	resp, err := j.client.CompleteWithMessages(ctx, remoteJudgeSystemPrompt, []llm.Message{{Role: "user", Content: msg}})
	if err != nil {
		return VerdictError, fmt.Errorf("askeval: remote judge: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(resp)) {
	case string(VerdictSupported):
		return VerdictSupported, nil
	case string(VerdictUnsupported):
		return VerdictUnsupported, nil
	default:
		return VerdictError, fmt.Errorf("askeval: remote judge: unrecognized response %q", firstBytes(resp, 80))
	}
}
