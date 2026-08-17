package worker

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestParseRelationActionResponse_LogDoesNotLeakRawResponse asserts the parse
// failure path never writes raw LLM output to the log (issue #194: real names,
// phone numbers and e-mail addresses were landing in the "response" field).
//
// Only a length and a content-free shape fingerprint may be logged.
//
// Not parallel: it swaps the process-wide default slog handler.
func TestParseRelationActionResponse_LogDoesNotLeakRawResponse(t *testing.T) {
	// Fabricated stand-ins — never real personal data.
	const (
		name  = "DUMMYNAME-ALPHA"
		phone = "010-0000-0000"
		mail  = "dummy-alpha@example.invalid"
	)
	raw := `{"entities":[{"name":"` + name + `","phone":"` + phone + `","email":"` + mail + `"` +
		strings.Repeat(" ", 300) // long enough to exercise any truncation branch

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if _, err := parseRelationActionResponse(raw); err == nil {
		t.Fatal("expected a parse error for truncated JSON")
	}

	logged := buf.String()
	if logged == "" {
		t.Fatal("expected a warning to be logged")
	}
	for _, secret := range []string{name, phone, mail} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaks raw LLM response (%q): %s", secret, logged)
		}
	}
	if strings.Contains(logged, "response=") {
		t.Fatalf("the raw \"response\" log field must be gone entirely: %s", logged)
	}
	if !strings.Contains(logged, "response_len") {
		t.Fatalf("log should still record the response length for diagnosis: %s", logged)
	}
}

// TestParseRelationActionResponse_LogsEmptyResponseShape covers the failure
// mode that motivated this change: the model returned a zero-length body, and
// the log said only "failed to parse LLM JSON" with an empty response field.
func TestParseRelationActionResponse_LogsEmptyResponseShape(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if _, err := parseRelationActionResponse(""); err == nil {
		t.Fatal("expected a parse error for an empty response")
	}
	logged := buf.String()
	if !strings.Contains(logged, "empty") {
		t.Fatalf("an empty completion must be identifiable from the log alone: %s", logged)
	}
}
