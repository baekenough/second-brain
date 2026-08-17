package config

import "testing"

// TestLoad_LLMThinking verifies LLM_THINKING parsing.
//
// Default is "disabled": deepseek-v4-flash bills reasoning_content against
// max_tokens and returns an empty completion (finish_reason=length) on
// documents whose reasoning runs long — the observed cause of ~16% extraction
// failures. Only the literal "enabled" turns reasoning back on.
func TestLoad_LLMThinking(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   string
	}{
		{name: "default_when_unset", unset: true, want: "disabled"},
		{name: "explicit_disabled", envVal: "disabled", want: "disabled"},
		{name: "explicit_enabled", envVal: "enabled", want: "enabled"},
		{name: "case_insensitive_enabled", envVal: "Enabled", want: "enabled"},
		{name: "whitespace_trimmed", envVal: " enabled ", want: "enabled"},
		{name: "empty_uses_default", envVal: "", want: "disabled"},
		{name: "unknown_value_uses_default", envVal: "low", want: "disabled"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "LLM_THINKING")
			} else {
				setenv(t, "LLM_THINKING", tc.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.LLMThinking != tc.want {
				t.Errorf("LLMThinking = %q, want %q", cfg.LLMThinking, tc.want)
			}
		})
	}
}
