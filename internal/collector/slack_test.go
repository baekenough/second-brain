package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestParseRetryAfter verifies all documented input formats.
func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  time.Duration
	}{
		{
			name:  "empty string returns zero",
			input: "",
			want:  0,
		},
		{
			name:  "integer 1 second",
			input: "1",
			want:  1 * time.Second,
		},
		{
			name:  "integer 60 seconds",
			input: "60",
			want:  60 * time.Second,
		},
		{
			name:  "integer zero",
			input: "0",
			want:  0,
		},
		{
			name:  "invalid string returns zero",
			input: "not-a-number",
			want:  0,
		},
		{
			name:  "HTTP date in the past returns zero",
			input: "Mon, 01 Jan 2000 00:00:00 GMT",
			want:  0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseRetryAfter(tc.input)
			if got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestParseRetryAfter_HTTPDateFuture verifies a future HTTP-date header is
// parsed and returns a positive duration.
func TestParseRetryAfter_HTTPDateFuture(t *testing.T) {
	t.Parallel()

	future := time.Now().Add(5 * time.Second)
	h := future.UTC().Format(http.TimeFormat)
	d := parseRetryAfter(h)
	if d <= 0 {
		t.Errorf("parseRetryAfter(%q) = %v, want positive duration", h, d)
	}
}

// TestDoWithBackoff_429ThenSuccess simulates a server that returns 429 with
// Retry-After: 0 on the first request and 200 on the second. The collector
// must succeed and make exactly two HTTP calls.
func TestDoWithBackoff_429ThenSuccess(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			// First call: rate-limited, respond immediately (Retry-After: 0).
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewSlackCollector("test-token", "T123")
	c.client = srv.Client()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/test", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := c.doWithBackoff(t.Context(), req)
	if err != nil {
		t.Fatalf("doWithBackoff returned error: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("got status %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server called %d times, want 2", n)
	}
}

// TestDoWithBackoff_MaxRetriesExceeded confirms that after maxRetries+1
// consecutive 429 responses, doWithBackoff returns a non-nil error.
// Retry-After is set to "1" (minimum non-zero) to avoid exponential fallback
// and keep total test time bounded (maxRetries * 1s = 5s).
func TestDoWithBackoff_MaxRetriesExceeded(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Use Retry-After: 1 so each retry waits 1s instead of triggering
		// exponential backoff, keeping total test time at ~5s.
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewSlackCollector("test-token", "T123")
	c.client = srv.Client()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/test", nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.doWithBackoff(t.Context(), req)
	if err == nil {
		t.Fatal("expected error after max retries, got nil")
	}

	// maxRetries is 5, so the loop runs attempts 0..5 = 6 calls total.
	const wantCalls = 6
	if n := calls.Load(); n != wantCalls {
		t.Errorf("server called %d times, want %d", n, wantCalls)
	}
}

// TestDoWithBackoff_ContextCancel verifies that a context cancellation during
// the backoff wait causes doWithBackoff to return immediately with ctx.Err().
func TestDoWithBackoff_ContextCancel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always respond 429 with a long Retry-After so the test would stall
		// if context cancellation is not respected.
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewSlackCollector("test-token", "T123")
	c.client = srv.Client()

	ctx, cancel := context.WithCancel(t.Context())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/test", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Cancel the context shortly after the first 429 is received.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err = c.doWithBackoff(ctx, req)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after context cancel, got nil")
	}
	// Should return well before the 30-second Retry-After delay.
	if elapsed > 5*time.Second {
		t.Errorf("doWithBackoff took %v, expected fast return on context cancel", elapsed)
	}
}
