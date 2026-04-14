package search_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/search"
)

// mockCompleter implements llm.Completer for unit testing.
type mockCompleter struct {
	enabled  bool
	response string
	err      error
}

func (m *mockCompleter) Enabled() bool { return m.enabled }

func (m *mockCompleter) CompleteWithMessages(_ context.Context, _ string, _ []llm.Message) (string, error) {
	return m.response, m.err
}

// Compile-time assertion: mockCompleter satisfies llm.Completer.
var _ llm.Completer = (*mockCompleter)(nil)

func TestExpand_NilClient(t *testing.T) {
	t.Parallel()

	const query = "what is kubernetes?"
	got := search.Expand(context.Background(), nil, query)
	if got != query {
		t.Fatalf("nil client: want original query %q, got %q", query, got)
	}
}

func TestExpand_DisabledClient(t *testing.T) {
	t.Parallel()

	const query = "explain goroutines"
	client := &mockCompleter{enabled: false}
	got := search.Expand(context.Background(), client, query)
	if got != query {
		t.Fatalf("disabled client: want original query %q, got %q", query, got)
	}
}

func TestExpand_Success(t *testing.T) {
	t.Parallel()

	const (
		query    = "what is a context deadline?"
		expanded = "A context deadline is a specific point in time after which a context is automatically cancelled."
	)
	client := &mockCompleter{enabled: true, response: expanded}
	got := search.Expand(context.Background(), client, query)

	if !strings.HasPrefix(got, query) {
		t.Fatalf("expanded result should start with original query; got %q", got)
	}
	if !strings.Contains(got, expanded) {
		t.Fatalf("expanded result should contain LLM response; got %q", got)
	}
	// Verify the two parts are separated by a blank line.
	if !strings.Contains(got, "\n\n") {
		t.Fatalf("expanded result should have \\n\\n separator; got %q", got)
	}
}

func TestExpand_LLMError(t *testing.T) {
	t.Parallel()

	const query = "how does TLS handshake work?"
	client := &mockCompleter{
		enabled: true,
		err:     errors.New("llm: upstream timeout"),
	}
	got := search.Expand(context.Background(), client, query)
	if got != query {
		t.Fatalf("on LLM error: want original query %q, got %q", query, got)
	}
}

func TestExpand_EmptyResponse(t *testing.T) {
	t.Parallel()

	const query = "what is raft consensus?"
	client := &mockCompleter{enabled: true, response: ""}
	got := search.Expand(context.Background(), client, query)
	if got != query {
		t.Fatalf("on empty response: want original query %q, got %q", query, got)
	}
}
