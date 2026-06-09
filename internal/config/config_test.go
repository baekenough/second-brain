package config

import (
	"os"
	"testing"
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
		name    string
		envVal  string
		unset   bool
		want    bool
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
