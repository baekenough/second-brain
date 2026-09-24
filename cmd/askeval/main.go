// Command askeval runs issue #266's offline end-to-end evaluation of
// POST /api/v1/ask against the fixture corpus in eval/ask/fixtures (or a
// directory given via --fixtures). It never makes a network call: the LLM,
// document search, and chunk search are all in-process fakes
// (internal/askeval) driving the REAL /ask handler through httptest.
//
// Usage:
//
//	go run ./cmd/askeval --out /tmp/report.json
//	go run ./cmd/askeval --out /tmp/candidate.json --baseline /tmp/report.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/baekenough/second-brain/internal/askeval"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("askeval", flag.ContinueOnError)
	fixturesDir := fs.String("fixtures", "eval/ask/fixtures", "directory of *.json fixture files")
	out := fs.String("out", "", "write the JSON report to this path (optional; a text summary always prints to stdout)")
	baseline := fs.String("baseline", "", "path to a prior JSON report (from --out) to diff this run against")
	llmMode := fs.String("llm", "scripted", "scripted|configured — issue #266 scope only implements scripted (deterministic, offline); configured is reserved for future work")
	judgeMode := fs.String("judge", "off", "off|shadow — a judge is NEVER a pass gate; shadow only records disagreement for future analysis")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *llmMode != "scripted" {
		fmt.Fprintln(os.Stderr, "askeval: --llm=configured is not implemented by this runner (issue #266 scope is offline/scripted only)")
		return 2
	}
	if *judgeMode != "off" && *judgeMode != "shadow" {
		fmt.Fprintln(os.Stderr, "askeval: --judge must be \"off\" or \"shadow\"")
		return 2
	}

	fixtures, err := askeval.Load(*fixturesDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "askeval:", err)
		return 1
	}

	results := askeval.Run(context.Background(), fixtures, askeval.DefaultRunOptions())

	prov := askeval.Provenance{
		GitRev:         gitRev(),
		FixtureSetHash: askeval.FixtureSetHash(fixtures),
		Cases:          len(fixtures),
		Mode:           *llmMode,
		JudgeMode:      *judgeMode,
	}
	report := askeval.BuildReport(results, prov)
	fmt.Print(report.Text())

	var harnessErrors []string
	for _, c := range report.Cases {
		if c.Error != "" {
			harnessErrors = append(harnessErrors, c.ID+": "+c.Error)
		}
	}
	if len(harnessErrors) > 0 {
		fmt.Fprintln(os.Stderr, "askeval: harness errors (fixture/config bugs, not pipeline findings):")
		for _, e := range harnessErrors {
			fmt.Fprintln(os.Stderr, "  "+e)
		}
	}

	if *baseline != "" {
		baseReport, err := loadReport(*baseline)
		if err != nil {
			fmt.Fprintln(os.Stderr, "askeval: --baseline:", err)
			return 1
		}
		diff, err := askeval.DiffReports(baseReport, report)
		if err != nil {
			fmt.Fprintln(os.Stderr, "askeval:", err)
			return 1
		}
		fmt.Println("diff vs baseline:", diff.Summary)
		if len(diff.Regressed) > 0 {
			fmt.Println("  regressed:", strings.Join(diff.Regressed, ", "))
		}
		if len(diff.Improved) > 0 {
			fmt.Println("  improved:", strings.Join(diff.Improved, ", "))
		}
	}

	if *out != "" {
		b, err := report.JSON()
		if err != nil {
			fmt.Fprintln(os.Stderr, "askeval:", err)
			return 1
		}
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "askeval:", err)
			return 1
		}
	}

	// This runner's non-zero exit is reserved for HARNESS failures (a
	// fixture/config bug that means nothing was measured) — never for a
	// pipeline-quality finding. A known-baseline metric failure (e.g. the
	// committed call_transcript_mid_late fixtures) is exactly what this
	// command exists to surface, and must not make itself impossible to
	// run in CI just by existing. Inspect report.FailingCaseIDs (in --out)
	// to gate on specific fixtures if a caller wants that.
	if len(harnessErrors) > 0 {
		return 1
	}
	return 0
}

func gitRev() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func loadReport(path string) (askeval.Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return askeval.Report{}, err
	}
	var r askeval.Report
	if err := json.Unmarshal(b, &r); err != nil {
		return askeval.Report{}, err
	}
	return r, nil
}
