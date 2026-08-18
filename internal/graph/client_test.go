package graph

import (
	"context"
	"testing"
	"time"
)

// TestNew_RejectsEmptyURI and TestNew_RejectsEmptyPassword pin the two
// argument checks that must happen BEFORE any network call: a misconfigured
// deployment has to fail loudly at wiring time rather than construct a driver
// that silently points at nothing.
func TestNew_RejectsEmptyURI(t *testing.T) {
	if _, err := New(context.Background(), Config{Username: "neo4j", Password: "x"}); err == nil {
		t.Fatal("New with empty URI: want error, got nil")
	}
}

func TestNew_RejectsEmptyPassword(t *testing.T) {
	if _, err := New(context.Background(), Config{URI: "bolt://localhost:7687", Username: "neo4j"}); err == nil {
		t.Fatal("New with empty password: want error, got nil")
	}
}

// TestWithDefaults covers the fallbacks so a caller that only sets URI and
// password still gets a bounded pool and timeout.
func TestWithDefaults(t *testing.T) {
	got := withDefaults(Config{URI: "bolt://localhost:7687", Password: "x"})
	if got.Username != "neo4j" {
		t.Errorf("Username = %q, want %q", got.Username, "neo4j")
	}
	if got.Timeout != defaultGraphTimeout {
		t.Errorf("Timeout = %v, want %v", got.Timeout, defaultGraphTimeout)
	}
	if got.MaxPoolSize != defaultMaxPoolSize {
		t.Errorf("MaxPoolSize = %d, want %d", got.MaxPoolSize, defaultMaxPoolSize)
	}

	explicit := withDefaults(Config{URI: "bolt://localhost:7687", Password: "x", Username: "u", Timeout: time.Second, MaxPoolSize: 3})
	if explicit.Username != "u" || explicit.Timeout != time.Second || explicit.MaxPoolSize != 3 {
		t.Errorf("withDefaults overwrote explicit values: %+v", explicit)
	}
}
