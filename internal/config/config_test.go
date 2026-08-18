package config

import (
	"os"
	"testing"
	"time"
)

// setenv is a test helper that sets env vars and registers a cleanup to restore them.
func setenv(t *testing.T, key, value string) {
	t.Helper()
	prev, existed := os.LookupEnv(key)
	if err := os.Setenv(key, value); err != nil {
		t.Fatalf("setenv %s=%q: %v", key, value, err)
	}
	t.Cleanup(func() {
		if existed {
			os.Setenv(key, prev) //nolint:errcheck
		} else {
			os.Unsetenv(key) //nolint:errcheck
		}
	})
}

// unsetenv is a test helper that unsets an env var and restores it on cleanup.
func unsetenv(t *testing.T, key string) {
	t.Helper()
	prev, existed := os.LookupEnv(key)
	os.Unsetenv(key) //nolint:errcheck
	t.Cleanup(func() {
		if existed {
			os.Setenv(key, prev) //nolint:errcheck
		}
	})
}

// TestLoad_SummarizerBackfillEnabled verifies SUMMARIZER_BACKFILL_ENABLED parsing.
func TestLoad_SummarizerBackfillEnabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   bool
	}{
		{name: "default_when_unset", unset: true, want: true},
		{name: "explicit_true", envVal: "true", want: true},
		{name: "explicit_false", envVal: "false", want: false},
		{name: "numeric_0", envVal: "0", want: false},
		{name: "numeric_1", envVal: "1", want: true}, // only "false"/"0" → disable
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// NOTE: t.Parallel() omitted here because Load() reads process-global
			// env vars; parallelising would require per-test env isolation via
			// individual goroutines with lock, which is more complexity than needed
			// for this simple config test.
			if tc.unset {
				unsetenv(t, "SUMMARIZER_BACKFILL_ENABLED")
			} else {
				setenv(t, "SUMMARIZER_BACKFILL_ENABLED", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.SummarizerBackfillEnabled != tc.want {
				t.Errorf("SummarizerBackfillEnabled = %v, want %v", cfg.SummarizerBackfillEnabled, tc.want)
			}
		})
	}
}

// TestLoad_SummarizerBatchSize verifies SUMMARIZER_BATCH_SIZE parsing.
func TestLoad_SummarizerBatchSize(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   int
	}{
		{name: "default_when_unset", unset: true, want: 50},
		{name: "explicit_100", envVal: "100", want: 100},
		{name: "invalid_string_uses_default", envVal: "notanumber", want: 50},
		{name: "zero_uses_default", envVal: "0", want: 50},
		{name: "negative_uses_default", envVal: "-1", want: 50},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "SUMMARIZER_BATCH_SIZE")
			} else {
				setenv(t, "SUMMARIZER_BATCH_SIZE", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.SummarizerBatchSize != tc.want {
				t.Errorf("SummarizerBatchSize = %d, want %d", cfg.SummarizerBatchSize, tc.want)
			}
		})
	}
}

// TestLoad_SummarizerInterval verifies SUMMARIZER_INTERVAL parsing.
func TestLoad_SummarizerInterval(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   time.Duration
	}{
		{name: "default_when_unset", unset: true, want: 30 * time.Second},
		{name: "explicit_1m", envVal: "1m", want: time.Minute},
		{name: "invalid_string_uses_default", envVal: "notaduration", want: 30 * time.Second},
		{name: "zero_uses_default", envVal: "0s", want: 30 * time.Second},
		{name: "negative_uses_default", envVal: "-5s", want: 30 * time.Second},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "SUMMARIZER_INTERVAL")
			} else {
				setenv(t, "SUMMARIZER_INTERVAL", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.SummarizerInterval != tc.want {
				t.Errorf("SummarizerInterval = %v, want %v", cfg.SummarizerInterval, tc.want)
			}
		})
	}
}

// TestLoad_SummarizerDocTimeout verifies SUMMARIZER_DOC_TIMEOUT parsing.
func TestLoad_SummarizerDocTimeout(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   time.Duration
	}{
		{name: "default_when_unset", unset: true, want: 30 * time.Second},
		{name: "explicit_45s", envVal: "45s", want: 45 * time.Second},
		{name: "invalid_string_uses_default", envVal: "notaduration", want: 30 * time.Second},
		{name: "zero_uses_default", envVal: "0s", want: 30 * time.Second},
		{name: "negative_uses_default", envVal: "-5s", want: 30 * time.Second},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "SUMMARIZER_DOC_TIMEOUT")
			} else {
				setenv(t, "SUMMARIZER_DOC_TIMEOUT", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.SummarizerDocTimeout != tc.want {
				t.Errorf("SummarizerDocTimeout = %v, want %v", cfg.SummarizerDocTimeout, tc.want)
			}
		})
	}
}

// TestLoad_SummarizerConcurrency verifies SUMMARIZER_CONCURRENCY parsing.
func TestLoad_SummarizerConcurrency(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   int
	}{
		{name: "default_when_unset", unset: true, want: 5},
		{name: "explicit_10", envVal: "10", want: 10},
		{name: "invalid_string_uses_default", envVal: "notanumber", want: 5},
		{name: "zero_uses_default", envVal: "0", want: 5},
		{name: "negative_uses_default", envVal: "-1", want: 5},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "SUMMARIZER_CONCURRENCY")
			} else {
				setenv(t, "SUMMARIZER_CONCURRENCY", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.SummarizerConcurrency != tc.want {
				t.Errorf("SummarizerConcurrency = %d, want %d", cfg.SummarizerConcurrency, tc.want)
			}
		})
	}
}

// TestLoad_GmailMaxMessages verifies GMAIL_MAX_MESSAGES parsing.
func TestLoad_GmailMaxMessages(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   int
	}{
		{name: "default_when_unset", unset: true, want: 50000},
		{name: "explicit_100000", envVal: "100000", want: 100000},
		{name: "zero_means_unlimited", envVal: "0", want: 0},
		{name: "invalid_string_uses_default", envVal: "notanumber", want: 50000},
		{name: "negative_uses_default", envVal: "-1", want: 50000},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "GMAIL_MAX_MESSAGES")
			} else {
				setenv(t, "GMAIL_MAX_MESSAGES", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.GmailMaxMessages != tc.want {
				t.Errorf("GmailMaxMessages = %d, want %d", cfg.GmailMaxMessages, tc.want)
			}
		})
	}
}

// TestLoad_WhisperMaxFileBytes verifies WHISPER_MAX_FILE_BYTES parsing.
func TestLoad_WhisperMaxFileBytes(t *testing.T) {
	t.Parallel()

	const defaultCap = int64(100 << 20) // 100 MiB

	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   int64
	}{
		{name: "default_when_unset", unset: true, want: defaultCap},
		{name: "explicit_200mib", envVal: "209715200", want: 209715200},
		{name: "zero_means_unlimited", envVal: "0", want: 0},
		{name: "invalid_string_uses_default", envVal: "notanumber", want: defaultCap},
		{name: "negative_uses_default", envVal: "-1", want: defaultCap},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "WHISPER_MAX_FILE_BYTES")
			} else {
				setenv(t, "WHISPER_MAX_FILE_BYTES", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.WhisperMaxFileBytes != tc.want {
				t.Errorf("WhisperMaxFileBytes = %d, want %d", cfg.WhisperMaxFileBytes, tc.want)
			}
		})
	}
}

// TestLoad_CalendarLookbehindDays_Default verifies the default is 365.
func TestLoad_CalendarLookbehindDays_Default(t *testing.T) {
	unsetenv(t, "CALENDAR_LOOKBEHIND_DAYS")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CalendarLookbehindDays != 365 {
		t.Errorf("CalendarLookbehindDays default = %d, want 365", cfg.CalendarLookbehindDays)
	}
}

// TestLoad_CalendarLookbehindDays_Override verifies env override still works.
func TestLoad_CalendarLookbehindDays_Override(t *testing.T) {
	setenv(t, "CALENDAR_LOOKBEHIND_DAYS", "90")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CalendarLookbehindDays != 90 {
		t.Errorf("CalendarLookbehindDays = %d, want 90", cfg.CalendarLookbehindDays)
	}
}

// TestLoad_CollectorCutover verifies COLLECTOR_CUTOVER RFC3339 parsing.
func TestLoad_CollectorCutover(t *testing.T) {
	t.Parallel()

	validCutover := "2025-01-01T00:00:00Z"
	wantTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		envVal  string
		unset   bool
		wantNil bool // true when we expect zero time
		want    time.Time
	}{
		{name: "unset_returns_zero", unset: true, wantNil: true},
		{name: "empty_returns_zero", envVal: "", wantNil: true},
		{name: "valid_rfc3339", envVal: validCutover, want: wantTime},
		{name: "invalid_string_returns_zero", envVal: "not-a-date", wantNil: true},
		{name: "non_rfc3339_date_only_returns_zero", envVal: "2025-01-01", wantNil: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// NOTE: t.Parallel() omitted — Load() reads process-global env vars.
			if tc.unset {
				unsetenv(t, "COLLECTOR_CUTOVER")
			} else {
				setenv(t, "COLLECTOR_CUTOVER", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if tc.wantNil {
				if !cfg.CollectorCutover.IsZero() {
					t.Errorf("CollectorCutover = %v, want zero time", cfg.CollectorCutover)
				}
				return
			}
			if !cfg.CollectorCutover.Equal(tc.want) {
				t.Errorf("CollectorCutover = %v, want %v", cfg.CollectorCutover, tc.want)
			}
		})
	}
}

// TestLoad_LLMTimeoutSeconds verifies LLM_TIMEOUT_SECONDS parsing.
func TestLoad_LLMTimeoutSeconds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   int
	}{
		{name: "default_when_unset", unset: true, want: 120},
		{name: "explicit_300", envVal: "300", want: 300},
		{name: "explicit_30", envVal: "30", want: 30},
		{name: "invalid_string_uses_default", envVal: "notanumber", want: 120},
		{name: "zero_uses_default", envVal: "0", want: 120}, // 0 is not > 0, keeps default
		{name: "negative_uses_default", envVal: "-5", want: 120},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "LLM_TIMEOUT_SECONDS")
			} else {
				setenv(t, "LLM_TIMEOUT_SECONDS", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.LLMTimeoutSeconds != tc.want {
				t.Errorf("LLMTimeoutSeconds = %d, want %d", cfg.LLMTimeoutSeconds, tc.want)
			}
		})
	}
}

// TestLoad_PIIRedactionEnabled verifies PII_REDACTION_ENABLED parsing
// (issue #163/#165/#167 policy reversal — default false, unlike the
// "false"/"0"-only-disables pattern used by SUMMARIZER_BACKFILL_ENABLED: this
// flag follows the simpler "only literal 'true' enables" convention already
// used by WHISPER_CLOUD_ALLOWED / DIARIZATION_ENABLED).
func TestLoad_PIIRedactionEnabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   bool
	}{
		{name: "default_when_unset", unset: true, want: false},
		{name: "explicit_true", envVal: "true", want: true},
		{name: "explicit_false", envVal: "false", want: false},
		{name: "empty_string_disabled", envVal: "", want: false},
		{name: "invalid_value_disabled", envVal: "yes", want: false}, // only literal "true" enables
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "PII_REDACTION_ENABLED")
			} else {
				setenv(t, "PII_REDACTION_ENABLED", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.PIIRedactionEnabled != tc.want {
				t.Errorf("PIIRedactionEnabled = %v, want %v", cfg.PIIRedactionEnabled, tc.want)
			}
		})
	}
}

// TestLoad_PIINumberHashingEnabled verifies PII_NUMBER_HASHING_ENABLED
// parsing (issue #164 policy reversal — default false).
func TestLoad_PIINumberHashingEnabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   bool
	}{
		{name: "default_when_unset", unset: true, want: false},
		{name: "explicit_true", envVal: "true", want: true},
		{name: "explicit_false", envVal: "false", want: false},
		{name: "invalid_value_disabled", envVal: "1", want: false}, // only literal "true" enables
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "PII_NUMBER_HASHING_ENABLED")
			} else {
				setenv(t, "PII_NUMBER_HASHING_ENABLED", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.PIINumberHashingEnabled != tc.want {
				t.Errorf("PIINumberHashingEnabled = %v, want %v", cfg.PIINumberHashingEnabled, tc.want)
			}
		})
	}
}

// TestLoad_ActionsAPIEnabled verifies ACTIONS_API_ENABLED parsing (Task 12 —
// feature flag default MUST be false, since the missing route (404) IS the
// rollback mechanism for the actions/briefing feature).
func TestLoad_ActionsAPIEnabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   bool
	}{
		{name: "default_when_unset", unset: true, want: false},
		{name: "explicit_true", envVal: "true", want: true},
		{name: "explicit_false", envVal: "false", want: false},
		{name: "invalid_value_disabled", envVal: "1", want: false}, // only literal "true" enables
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "ACTIONS_API_ENABLED")
			} else {
				setenv(t, "ACTIONS_API_ENABLED", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.ActionsAPIEnabled != tc.want {
				t.Errorf("ActionsAPIEnabled = %v, want %v", cfg.ActionsAPIEnabled, tc.want)
			}
		})
	}
}

// TestLoad_BriefingEnabled verifies BRIEFING_ENABLED parsing (Task 12 —
// default false; whether it takes effect also depends on ActionsAPIEnabled,
// which is wired in cmd/server/main.go, not here).
func TestLoad_BriefingEnabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   bool
	}{
		{name: "default_when_unset", unset: true, want: false},
		{name: "explicit_true", envVal: "true", want: true},
		{name: "explicit_false", envVal: "false", want: false},
		{name: "invalid_value_disabled", envVal: "yes", want: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "BRIEFING_ENABLED")
			} else {
				setenv(t, "BRIEFING_ENABLED", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.BriefingEnabled != tc.want {
				t.Errorf("BriefingEnabled = %v, want %v", cfg.BriefingEnabled, tc.want)
			}
		})
	}
}

// TestLoad_BriefingMaxActions verifies BRIEFING_MAX_ACTIONS parsing.
func TestLoad_BriefingMaxActions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   int
	}{
		{name: "default_when_unset", unset: true, want: 40},
		{name: "explicit_100", envVal: "100", want: 100},
		{name: "invalid_string_uses_default", envVal: "notanumber", want: 40},
		{name: "zero_uses_default", envVal: "0", want: 40},
		{name: "negative_uses_default", envVal: "-1", want: 40},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "BRIEFING_MAX_ACTIONS")
			} else {
				setenv(t, "BRIEFING_MAX_ACTIONS", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.BriefingMaxActions != tc.want {
				t.Errorf("BriefingMaxActions = %d, want %d", cfg.BriefingMaxActions, tc.want)
			}
		})
	}
}

// TestLoad_UserEmailAddresses verifies USER_EMAIL_ADDRESSES parsing (reuses
// splitCSV, same convention as FILESYSTEM_EXCLUDE_DIRS).
func TestLoad_UserEmailAddresses(t *testing.T) {
	setenv(t, "USER_EMAIL_ADDRESSES", "me@example.com, alias@example.com ,")
	defer unsetenv(t, "USER_EMAIL_ADDRESSES")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	want := []string{"me@example.com", "alias@example.com"}
	if len(cfg.UserEmailAddresses) != len(want) {
		t.Fatalf("UserEmailAddresses = %v, want %v", cfg.UserEmailAddresses, want)
	}
	for i, w := range want {
		if cfg.UserEmailAddresses[i] != w {
			t.Errorf("UserEmailAddresses[%d] = %q, want %q", i, cfg.UserEmailAddresses[i], w)
		}
	}
}

// TestLoad_BriefingTimeoutSeconds verifies BRIEFING_TIMEOUT_SECONDS parsing.
// The default must be comfortably above the observed LLM latency: production
// measurement showed the briefing LLM call exceeding the old hardcoded 30s
// ceiling on every request, which silently degraded the endpoint to the
// aggregate-only fallback (degraded=true, one sentence, dropped_count=0).
func TestLoad_BriefingTimeoutSeconds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   time.Duration
	}{
		{name: "default_when_unset", unset: true, want: 120 * time.Second},
		{name: "explicit_45", envVal: "45", want: 45 * time.Second},
		{name: "explicit_300", envVal: "300", want: 300 * time.Second},
		{name: "invalid_string_uses_default", envVal: "notanumber", want: 120 * time.Second},
		{name: "zero_uses_default", envVal: "0", want: 120 * time.Second},
		{name: "negative_uses_default", envVal: "-1", want: 120 * time.Second},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "BRIEFING_TIMEOUT_SECONDS")
			} else {
				setenv(t, "BRIEFING_TIMEOUT_SECONDS", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.BriefingTimeout != tc.want {
				t.Errorf("BriefingTimeout = %v, want %v", cfg.BriefingTimeout, tc.want)
			}
			// The api package resolves the same value per request through the
			// exported helper; the two must not drift.
			if got := BriefingTimeout(); got != tc.want {
				t.Errorf("BriefingTimeout() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLoad_HTTPWriteTimeoutSeconds verifies HTTP_WRITE_TIMEOUT_SECONDS parsing.
// This is the global http.Server.WriteTimeout — a slow-client guard that must
// stay bounded, so 0/negative fall back to the default rather than meaning
// "unlimited".
func TestLoad_HTTPWriteTimeoutSeconds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   time.Duration
	}{
		{name: "default_when_unset", unset: true, want: 90 * time.Second},
		{name: "explicit_30", envVal: "30", want: 30 * time.Second},
		{name: "invalid_string_uses_default", envVal: "abc", want: 90 * time.Second},
		{name: "zero_uses_default", envVal: "0", want: 90 * time.Second},
		{name: "negative_uses_default", envVal: "-10", want: 90 * time.Second},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "HTTP_WRITE_TIMEOUT_SECONDS")
			} else {
				setenv(t, "HTTP_WRITE_TIMEOUT_SECONDS", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.HTTPWriteTimeout != tc.want {
				t.Errorf("HTTPWriteTimeout = %v, want %v", cfg.HTTPWriteTimeout, tc.want)
			}
		})
	}
}

// TestLoad_FeedbackEvidenceEnabled verifies FEEDBACK_EVIDENCE_ENABLED parsing.
// The accepted truthy set deliberately mirrors the api handler's own gate
// (1/true/yes/on, case-insensitive) so that wiring and handler cannot
// disagree about whether the feature is on.
func TestLoad_FeedbackEvidenceEnabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   bool
	}{
		{name: "default_off_when_unset", unset: true, want: false},
		{name: "empty_is_off", envVal: "", want: false},
		{name: "true_is_on", envVal: "true", want: true},
		{name: "TRUE_is_on", envVal: "TRUE", want: true},
		{name: "one_is_on", envVal: "1", want: true},
		{name: "yes_is_on", envVal: "yes", want: true},
		{name: "on_is_on", envVal: " on ", want: true},
		{name: "false_is_off", envVal: "false", want: false},
		{name: "garbage_is_off", envVal: "maybe", want: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "FEEDBACK_EVIDENCE_ENABLED")
			} else {
				setenv(t, "FEEDBACK_EVIDENCE_ENABLED", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.FeedbackEvidenceEnabled != tc.want {
				t.Errorf("FeedbackEvidenceEnabled = %v, want %v", cfg.FeedbackEvidenceEnabled, tc.want)
			}
		})
	}
}
