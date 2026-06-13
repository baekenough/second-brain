package collector

import (
	"log/slog"
	"strings"
	"time"
)

// parseISO8601Timestamp attempts to parse an ISO-8601 / RFC3339 timestamp
// string into a *time.Time. The returned pointer is nil when s is blank or
// cannot be parsed; callers must handle nil gracefully.
//
// Tried formats (in order):
//  1. RFC3339 with sub-second precision  — "2006-01-02T15:04:05.999999999Z07:00"
//  2. RFC3339 (standard)                 — "2006-01-02T15:04:05Z07:00"
//  3. Date-only                          — "2006-01-02"
//
// Previously exported as parseSecretaryTimestamp (secretary.go). Renamed to
// parseISO8601Timestamp and moved here so llm_memory.go can use it without
// depending on the decommissioned SecretaryCollector.
func parseISO8601Timestamp(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			utc := t.UTC()
			return &utc
		}
	}

	slog.Debug("collector: could not parse timestamp; occurred_at will be NULL",
		"timestamp", s)
	return nil
}

// parseSecretaryTimestamp is an alias retained for backward compatibility.
// New callers should use parseISO8601Timestamp directly.
func parseSecretaryTimestamp(s string) *time.Time {
	return parseISO8601Timestamp(s)
}
