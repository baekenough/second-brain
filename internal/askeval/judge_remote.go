package askeval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

// repoModule is this repository's own module path (go.mod's "module"
// line) — repoRoot's anchor for recognizing "the directory containing
// go.mod IS this repo" rather than some unrelated Go module that also
// happens to have an eval/ask/fixtures-shaped subtree.
const repoModule = "github.com/baekenough/second-brain"

// repoRoot locates this repository's root directory purely from this
// SOURCE FILE's own compile-time path (runtime.Caller), walking upward
// until it finds a go.mod whose module line is exactly repoModule — never
// from the process's current working directory, an environment variable,
// or an exec'd `git rev-parse --show-toplevel` (deep-verify #273 HIGH
// finding's own suggestion: "prefer no exec if a pure-Go approach is
// reliable"). A cwd- or env-based root is exactly the kind of thing an
// operator's mistake (or a malicious --fixtures value) could manipulate;
// this file's own path on disk cannot be influenced by either.
//
// Returns an error (never a best-guess fallback) when go.mod cannot be
// found above this file, or — deliberately — when this binary was built
// with `-trimpath`: runtime.Caller then returns a module-relative path
// with no corresponding real directory on this machine, so the go.mod walk
// fails closed rather than silently resolving to something unintended.
// requireFixturesUnderRepo's callers (NewRemoteClaimJudge) treat that
// failure exactly like every other guard failure: refuse to start.
func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("askeval: runtime.Caller failed while locating this repository's root")
	}
	dir := filepath.Dir(file) // .../internal/askeval
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && goModDeclaresModule(b, repoModule) {
			resolved, err := filepath.EvalSymlinks(dir)
			if err != nil {
				return "", fmt.Errorf("resolving repo root %q: %w", dir, err)
			}
			return resolved, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("askeval: no go.mod declaring module %q found above %s", repoModule, filepath.Dir(file))
		}
		dir = parent
	}
}

// goModDeclaresModule reports whether go.mod's content b declares exactly
// "module "+module on its first line (go.mod's module directive is always
// the first non-comment, non-blank line in every go.mod this repo's own
// tooling writes; a stricter full-file scan is unnecessary for a check
// that only needs to recognize THIS repository, not validate an arbitrary
// go.mod's syntax).
func goModDeclaresModule(b []byte, module string) bool {
	line, _, _ := bytes.Cut(b, []byte("\n"))
	return string(bytes.TrimSpace(line)) == "module "+module
}

// requireFixturesUnderRepo rejects any --fixtures path that does not
// resolve — after following every symlink in its own path AND in the
// repo's own eval/ask/fixtures path — into this repository's
// eval/ask/fixtures tree (issue #273's "합성 fixture만 쓴다"), independent
// of cmd/askeval's own --fixtures default.
//
// This replaces a plain filepath.Abs + strings.Contains(abs,
// "/eval/ask/fixtures") check (deep-verify #273 HIGH finding), which two
// distinct attacks could defeat:
//
//  1. A path OUTSIDE the repo whose trailing components merely happen to
//     spell "eval/ask/fixtures" (e.g. /tmp/evil/eval/ask/fixtures) —
//     contained the substring, so the old check passed it, even though it
//     shares nothing with this repo's actual fixtures tree.
//  2. A directory (or an individual *.json file inside an otherwise
//     legitimate-looking directory) that is a SYMLINK resolving outside
//     the repo — the old check never called filepath.EvalSymlinks, so it
//     judged the symlink's own (repo-shaped) path, never where it actually
//     points.
//
// Both are closed by resolving symlinks on both sides of the comparison
// and checking filepath.Rel for a "does not escape" result, plus rejecting
// any symlinked *.json entry found directly inside the (already-resolved)
// directory.
func requireFixturesUnderRepo(dir string) error {
	if dir == "" {
		return errors.New("fixtures dir is empty")
	}
	root, err := repoRoot()
	if err != nil {
		return fmt.Errorf("resolving this repository's root: %w", err)
	}
	fixturesRoot, err := filepath.EvalSymlinks(filepath.Join(root, "eval", "ask", "fixtures"))
	if err != nil {
		return fmt.Errorf("resolving this repository's eval/ask/fixtures directory: %w", err)
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("resolving fixtures dir %q: %w", dir, err)
	}
	if err := requireWithin(fixturesRoot, resolvedDir); err != nil {
		return fmt.Errorf("fixtures dir %q: %w", dir, err)
	}
	return rejectSymlinkedJSONOutside(resolvedDir, fixturesRoot)
}

// requireWithin returns an error unless target is root itself or a
// descendant of root, evaluated on filepath.Rel's result — the only
// unambiguous way to tell "inside" from "outside" once both paths have
// already been symlink-resolved and made absolute by their callers.
func requireWithin(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return fmt.Errorf("computing relative path to %q: %w", root, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("resolves to %q, outside %q", target, root)
	}
	return nil
}

// rejectSymlinkedJSONOutside inspects every direct *.json entry of dir
// (never a recursive walk — Load itself, fixture.go, only ever reads
// dir's own top-level *.json files, so nothing deeper needs checking) and
// refuses if any of them is a symlink whose resolved target escapes
// allowedRoot. os.ReadDir's DirEntry.Info() reports the entry itself
// (an Lstat, not a Stat) — exactly what is needed to detect a symlink
// without following it first.
func rejectSymlinkedJSONOutside(dir, allowedRoot string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading fixtures dir %q: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			return fmt.Errorf("stat %q: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolving symlink %q: %w", path, err)
		}
		if err := requireWithin(allowedRoot, target); err != nil {
			return fmt.Errorf("fixture file %q is a symlink that %w", path, err)
		}
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
