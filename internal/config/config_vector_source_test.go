package config

import (
	"strings"
	"testing"
)

// VECTOR_SOURCE decides who writes the vectors search reads, so a typo must
// not fall back to either answer: "ptha" leaving ingest embedding into the
// application's columns while the operator believes Ptah owns them is the
// failure this refusal prevents.
func TestLoad_VectorSource(t *testing.T) {
	cases := []struct {
		name   string
		envVal string
		unset  bool
		want   string
	}{
		{name: "unset reads the application's columns", unset: true, want: VectorSourceApp},
		{name: "app", envVal: "app", want: VectorSourceApp},
		{name: "ptah", envVal: "ptah", want: VectorSourcePtah},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetenv(t, "VECTOR_SOURCE")
			} else {
				setenv(t, "VECTOR_SOURCE", tc.envVal)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.VectorSource != tc.want {
				t.Errorf("VectorSource = %q, want %q", cfg.VectorSource, tc.want)
			}
		})
	}
}

func TestLoad_VectorSourceRefusesAnUnknownValue(t *testing.T) {
	setenv(t, "VECTOR_SOURCE", "ptha")
	cfg, err := Load()
	if err == nil {
		t.Fatalf("Load accepted VECTOR_SOURCE=ptha: %+v", cfg.VectorSource)
	}
	if !strings.Contains(err.Error(), `VECTOR_SOURCE "ptha"`) {
		t.Errorf("error = %v, want it to name the value", err)
	}
}
