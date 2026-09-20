package worker

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestEntityExtractionParseErrorDoesNotLogResponse(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	_, err := parseExtractionResponse("PRIVATE_PERSON_PHONE_SENTINEL malformed response")
	if err == nil {
		t.Fatal("expected parse error")
	}
	if strings.Contains(logs.String(), "PRIVATE_PERSON_PHONE_SENTINEL") {
		t.Fatal("raw response leaked into logs")
	}
	if !strings.Contains(logs.String(), "response_bytes=") {
		t.Fatal("missing safe length diagnostic")
	}
}
