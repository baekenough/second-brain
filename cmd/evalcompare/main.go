// Command evalcompare pairs two cmd/eval --dump v2 files (baseline vs
// candidate) by query_id and reports per-slice regressions with a
// fixed-seed paired bootstrap confidence interval (issue #269). It never
// opens a network connection or a database — it only reads the two dump
// files (and, optionally, a slice-label file) already on disk, which is
// what lets it run offline in CI.
//
// The output binary is named evalcompare, not eval, so it does not collide
// with the root eval/ directory that shadows `go build ./cmd/eval`'s
// default output name (issue #274).
//
// Exit codes:
//
//	0 — compared, no regression detected
//	1 — a regression was detected (see Report.Regressed)
//	2 — the two dumps cannot be compared at all (label_hash mismatch, a
//	    v1/no-header dump without --allow-v1, mismatched query sets, a
//	    failed run, or a corrupt dump whose stored ndcg10 disagrees with
//	    its own rank evidence)
//	3 — usage error (bad flags, unreadable file)
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/baekenough/second-brain/internal/evalcompare"
	"github.com/baekenough/second-brain/internal/evaldump"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("evalcompare", flag.ContinueOnError)
	fs.SetOutput(stderr)

	baselinePath := fs.String("baseline", "", "baseline --dump file from cmd/eval (required)")
	candidatePath := fs.String("candidate", "", "candidate --dump file from cmd/eval (required)")
	slicesPath := fs.String("slices", "",
		"optional slice-label file (query_id -> manual tags; see internal/evalcompare.Slices); "+
			"omit to compare only the derived answer_source/query_source groups")
	seed := fs.Uint64("seed", 1, "PCG seed for the paired bootstrap; fixed by default for reproducibility")
	iterations := fs.Int("iterations", 0, "bootstrap resamples per group/metric (default 10000)")
	minN := fs.Int("min-n", 0, "minimum paired queries before a group's verdict can be conclusive (default 20)")
	allowV1 := fs.Bool("allow-v1", false,
		"force an inconclusive compare of a v1 (no-header, provenance-unknown) dump instead of refusing it")
	format := fs.String("format", "text", "output format: text or json")

	if err := fs.Parse(args); err != nil {
		return 3
	}
	if *baselinePath == "" || *candidatePath == "" {
		fmt.Fprintln(stderr, "evalcompare: --baseline and --candidate are required")
		return 3
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "evalcompare: invalid --format %q (want text or json)\n", *format)
		return 3
	}

	baseline, err := evaldump.Read(*baselinePath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 3
	}
	candidate, err := evaldump.Read(*candidatePath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 3
	}

	var slices *evalcompare.Slices
	if *slicesPath != "" {
		slices, err = evalcompare.LoadSlices(*slicesPath)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 3
		}
	}

	report, err := evalcompare.Compare(baseline, candidate, slices, evalcompare.Options{
		Seed: *seed, Iterations: *iterations, MinN: *minN, AllowV1: *allowV1,
	})
	if err != nil {
		var invalidErr *evalcompare.InvalidError
		if errors.As(err, &invalidErr) {
			fmt.Fprintln(stderr, err)
			return 2
		}
		fmt.Fprintln(stderr, err)
		return 3
	}

	switch *format {
	case "json":
		body, jerr := report.JSON()
		if jerr != nil {
			fmt.Fprintln(stderr, jerr)
			return 3
		}
		stdout.Write(body)
	default:
		fmt.Fprint(stdout, report.Text())
	}

	if report.Regressed {
		return 1
	}
	return 0
}
