package main

import (
	"bytes"
	"strings"
	"testing"
)

func td(name string) string { return "testdata/" + name }

func TestRun_RegressionExitsOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--baseline", td("baseline.jsonl"),
		"--candidate", td("candidate_regressed.jsonl"),
		"--min-n", "2", "--seed", "1", "--iterations", "500",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "regressed") {
		t.Fatalf("stdout does not mention a regressed verdict:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "RESULT: regression detected") {
		t.Fatalf("stdout missing the RESULT line:\n%s", stdout.String())
	}
}

func TestRun_NoRegressionExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--baseline", td("baseline.jsonl"),
		"--candidate", td("candidate_same.jsonl"),
		"--min-n", "2", "--seed", "1", "--iterations", "500",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "RESULT: no regression detected") {
		t.Fatalf("stdout missing the RESULT line:\n%s", stdout.String())
	}
}

func TestRun_JSONFormat(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--baseline", td("baseline.jsonl"),
		"--candidate", td("candidate_same.jsonl"),
		"--min-n", "2", "--format", "json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout.String()), "{") {
		t.Fatalf("--format=json did not produce a JSON object:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"paired_queries": 2`) {
		t.Fatalf("json output missing paired_queries:\n%s", stdout.String())
	}
}

func TestRun_LabelHashMismatchExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--baseline", td("baseline.jsonl"),
		"--candidate", td("candidate_label_mismatch.jsonl"),
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (label_hash mismatch); stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "label_hash mismatch") {
		t.Fatalf("stderr does not explain the label_hash mismatch:\n%s", stderr.String())
	}
}

func TestRun_V1DumpWithoutAllowV1ExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--baseline", td("baseline.jsonl"),
		"--candidate", td("v1_no_header.jsonl"),
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (v1 dump without --allow-v1); stderr=%s", code, stderr.String())
	}
}

func TestRun_AllowV1ExitsZeroOrOneNeverTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--baseline", td("v1_no_header.jsonl"),
		"--candidate", td("v1_no_header.jsonl"),
		"--allow-v1", "--min-n", "1",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (identical v1 dumps, forced inconclusive, never a regression); stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "warning") {
		t.Fatalf("stdout missing the provenance-unknown warning:\n%s", stdout.String())
	}
}

func TestRun_MissingFlagsExitThree(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (missing --baseline/--candidate)", code)
	}
}

func TestRun_UnreadableFileExitsThree(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--baseline", td("does-not-exist.jsonl"), "--candidate", td("baseline.jsonl")}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (unreadable baseline file)", code)
	}
}

func TestRun_InvalidFormatExitsThree(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--baseline", td("baseline.jsonl"), "--candidate", td("candidate_same.jsonl"), "--format", "xml",
	}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (invalid --format)", code)
	}
}
