package collector_test

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/baekenough/second-brain/internal/collector"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// Exported test helpers — thin wrappers over package-internal functions that
// are exposed via export_test.go (created below) when the functions are
// unexported. Since the functions are in the same package under test, we use
// the collector_test package and rely on the testable export file.
// ---------------------------------------------------------------------------

// --- buildContextBlock ---

func TestBuildContextBlock_Empty(t *testing.T) {
	t.Parallel()

	result := collector.ExportBuildContextBlock(nil)
	if result != "" {
		t.Fatalf("expected empty string for nil results, got %q", result)
	}

	result = collector.ExportBuildContextBlock([]*model.SearchResult{})
	if result != "" {
		t.Fatalf("expected empty string for empty results, got %q", result)
	}
}

func TestBuildContextBlock_WithResults(t *testing.T) {
	t.Parallel()

	results := []*model.SearchResult{
		{
			Document: model.Document{
				SourceID: "src-1",
				Title:    "Title One",
				Content:  "Content of result one",
			},
		},
		{
			Document: model.Document{
				SourceID: "src-2",
				Title:    "Title Two",
				Content:  "Content of result two",
			},
		},
	}

	got := collector.ExportBuildContextBlock(results)
	if got == "" {
		t.Fatal("expected non-empty context block")
	}

	// Must contain both numbered entries.
	for _, want := range []string{"[1]", "[2]", "Title One", "Title Two", "src-1", "src-2"} {
		if !strings.Contains(got, want) {
			t.Errorf("context block missing %q\nfull output:\n%s", want, got)
		}
	}
}

func TestBuildContextBlock_TruncatesLongSnippets(t *testing.T) {
	t.Parallel()

	// Build a snippet that is 1000 runes — well over the 800-rune limit.
	longContent := strings.Repeat("あ", 1000) // multi-byte runes to test rune-boundary truncation

	results := []*model.SearchResult{
		{
			Document: model.Document{
				SourceID: "src-long",
				Title:    "Long Snippet",
				Content:  longContent,
			},
		},
	}

	got := collector.ExportBuildContextBlock(results)

	// The output must contain the truncation ellipsis.
	if !strings.Contains(got, "…") {
		t.Fatal("expected truncation ellipsis in context block")
	}

	// The full original content must NOT appear verbatim.
	if strings.Contains(got, longContent) {
		t.Fatal("full long content should not appear — expected truncation")
	}

	// Ensure the snippet portion is at most 800 runes (plus ellipsis and metadata).
	// Extract the content between the title line and the source line.
	lines := strings.Split(got, "\n")
	var snippetLines []string
	inSnippet := false
	for _, line := range lines {
		if strings.HasPrefix(line, "[1]") {
			inSnippet = true
			continue
		}
		if strings.HasPrefix(line, "출처:") {
			break
		}
		if inSnippet {
			snippetLines = append(snippetLines, line)
		}
	}
	snippetText := strings.Join(snippetLines, "\n")
	runeCount := utf8.RuneCountInString(snippetText)
	// 800 runes + 1 ellipsis = 801 rune cap for the snippet portion.
	if runeCount > 810 {
		t.Fatalf("snippet rune count %d exceeds expected cap (~801)", runeCount)
	}
}

func TestBuildContextBlock_TruncatesTotalCap(t *testing.T) {
	t.Parallel()

	// Create enough results to exceed the 12000-char total cap.
	// Each result: title (~20 chars) + 800-char snippet + source line (~20) + separator (~4) ≈ 850 chars.
	// 20 results * 850 ≈ 17000 chars — well above 12000.
	const numResults = 20
	results := make([]*model.SearchResult, numResults)
	for i := range results {
		results[i] = &model.SearchResult{
			Document: model.Document{
				SourceID: "src",
				Title:    "Title",
				Content:  strings.Repeat("x", 800),
			},
		}
	}

	got := collector.ExportBuildContextBlock(results)
	if len(got) > 12000 {
		t.Fatalf("context block length %d exceeds 12000-char cap", len(got))
	}

	// Must still have at least one entry.
	if !strings.Contains(got, "[1]") {
		t.Fatal("expected at least one numbered entry in context block")
	}
}

// --- stripMention ---

func TestStripMention(t *testing.T) {
	t.Parallel()

	const botID = "123456789"
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "plain mention",
			content: "<@123456789> hello",
			want:    " hello",
		},
		{
			name:    "nickname mention",
			content: "<@!123456789> hello",
			want:    " hello",
		},
		{
			name:    "multiple mentions",
			content: "<@123456789> <@!123456789> question",
			want:    "  question",
		},
		{
			name:    "no mention",
			content: "plain text",
			want:    "plain text",
		},
		{
			name:    "other user mention preserved",
			content: "<@999> message",
			want:    "<@999> message",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := collector.ExportStripMention(tc.content, botID)
			if got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
		})
	}
}

// --- splitForDiscord ---

func TestSplitForDiscord(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		text       string
		maxLen     int
		wantChunks int
	}{
		{
			name:       "short text fits in one chunk",
			text:       "hello world",
			maxLen:     2000,
			wantChunks: 1,
		},
		{
			name:       "exact length fits in one chunk",
			text:       strings.Repeat("a", 2000),
			maxLen:     2000,
			wantChunks: 1,
		},
		{
			name:       "slightly over splits into two",
			text:       strings.Repeat("a", 1999) + " " + strings.Repeat("b", 5),
			maxLen:     2000,
			wantChunks: 2,
		},
		{
			name:       "paragraph break preferred",
			text:       strings.Repeat("a", 1000) + "\n\n" + strings.Repeat("b", 1001),
			maxLen:     2000,
			wantChunks: 2,
		},
		{
			name:       "empty string",
			text:       "",
			maxLen:     2000,
			wantChunks: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			chunks := collector.ExportSplitForDiscord(tc.text, tc.maxLen)
			if len(chunks) != tc.wantChunks {
				t.Fatalf("want %d chunks, got %d: %v", tc.wantChunks, len(chunks), chunks)
			}
			// Every chunk must be within the max length.
			for i, chunk := range chunks {
				if utf8.RuneCountInString(chunk) > tc.maxLen {
					t.Errorf("chunk[%d] rune count %d exceeds maxLen %d",
						i, utf8.RuneCountInString(chunk), tc.maxLen)
				}
			}
			// Reassembled content must preserve all non-whitespace characters.
			// splitForDiscord calls TrimSpace on each chunk so leading/trailing
			// whitespace (including \n) may be stripped — compare only
			// non-whitespace rune counts.
			reassembled := strings.Join(chunks, "")
			stripWS := func(s string) string {
				var b strings.Builder
				for _, r := range s {
					if r != ' ' && r != '\n' && r != '\r' && r != '\t' {
						b.WriteRune(r)
					}
				}
				return b.String()
			}
			gotRunes := utf8.RuneCountInString(stripWS(reassembled))
			wantRunes := utf8.RuneCountInString(stripWS(tc.text))
			if gotRunes != wantRunes {
				t.Errorf("content loss detected: want %d non-whitespace runes, got %d",
					wantRunes, gotRunes)
			}
		})
	}
}

// --- buildConversationHistory ---

func TestBuildConversationHistory(t *testing.T) {
	t.Parallel()

	// We test the exported version of buildConversationHistory.
	const botID = "bot-999"

	cases := []struct {
		name      string
		msgs      []collector.TestInputMessage
		wantRoles []string
	}{
		{
			name: "bot messages become assistant role",
			msgs: []collector.TestInputMessage{
				{AuthorID: botID, Content: "I am the bot"},
			},
			wantRoles: []string{"assistant"},
		},
		{
			name: "user messages become user role",
			msgs: []collector.TestInputMessage{
				{AuthorID: "user-1", Content: "user question"},
			},
			wantRoles: []string{"user"},
		},
		{
			name: "mixed history preserves order and roles",
			msgs: []collector.TestInputMessage{
				{AuthorID: "user-1", Content: "first"},
				{AuthorID: botID, Content: "reply"},
				{AuthorID: "user-2", Content: "follow-up"},
			},
			wantRoles: []string{"user", "assistant", "user"},
		},
		{
			name: "empty messages are skipped",
			msgs: []collector.TestInputMessage{
				{AuthorID: "user-1", Content: ""},
				{AuthorID: "user-1", Content: "real"},
			},
			wantRoles: []string{"user"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := collector.ExportBuildConversationHistory(tc.msgs, botID)
			if len(got) != len(tc.wantRoles) {
				t.Fatalf("want %d messages, got %d", len(tc.wantRoles), len(got))
			}
			for i, want := range tc.wantRoles {
				if got[i].Role != want {
					t.Errorf("msgs[%d].Role: want %q, got %q", i, want, got[i].Role)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// #32 — buildReply contract: zero search results / search error must still
// call LLM (search is optional context, not a gate).
// ---------------------------------------------------------------------------

// mockSearcher satisfies the collector.Searcher interface for testing.
type mockSearcher struct {
	results []*model.SearchResult
	err     error
	calls   int
}

func (m *mockSearcher) Search(_ context.Context, _ model.SearchQuery) ([]*model.SearchResult, error) {
	m.calls++
	return m.results, m.err
}

// mockCompleter satisfies the llm.Completer interface for testing.
type mockCompleter struct {
	answer string
	err    error
	calls  int
}

func (m *mockCompleter) Enabled() bool { return true }
func (m *mockCompleter) CompleteWithMessages(_ context.Context, _ string, _ []llm.Message) (string, error) {
	m.calls++
	return m.answer, m.err
}

// Compile-time assertions.
var _ collector.Searcher = (*mockSearcher)(nil)
var _ llm.Completer = (*mockCompleter)(nil)

func TestBuildReply_ZeroSearchResults_StillCallsLLM(t *testing.T) {
	t.Parallel()

	searcher := &mockSearcher{results: nil, err: nil} // 0 results, no error
	completer := &mockCompleter{answer: "LLM answer for empty results"}

	answer := collector.ExportBuildReply(
		context.Background(),
		searcher,
		completer,
		"bot-id",
		"what is the answer?",
	)

	// The LLM must have been called exactly once.
	if completer.calls != 1 {
		t.Fatalf("LLM must be called once even with 0 search results, got %d calls", completer.calls)
	}

	// The response must be the LLM answer, not a fallback error.
	if answer != "LLM answer for empty results" {
		t.Fatalf("want LLM answer, got %q", answer)
	}
}

func TestBuildReply_SearchError_StillCallsLLM(t *testing.T) {
	t.Parallel()

	searcher := &mockSearcher{results: nil, err: context.DeadlineExceeded}
	completer := &mockCompleter{answer: "LLM answer despite search error"}

	answer := collector.ExportBuildReply(
		context.Background(),
		searcher,
		completer,
		"bot-id",
		"question after search failure",
	)

	// Search error is non-fatal — LLM must still be called.
	if completer.calls != 1 {
		t.Fatalf("LLM must be called once even when search errors, got %d calls", completer.calls)
	}
	if answer != "LLM answer despite search error" {
		t.Fatalf("want LLM answer, got %q", answer)
	}
}
