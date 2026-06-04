package chunker

import (
	"testing"

	"github.com/baekenough/second-brain/internal/model"
)

// TestSelectOptions asserts that SelectOptions returns the correct chunking
// strategy for each source type, providing a regression guard so that the
// long-form defaults (filesystem, notion) are never silently changed.
func TestSelectOptions(t *testing.T) {
	t.Parallel()

	// structuredContent has headings and paragraph breaks, activating
	// HeadingAware for long-form sources.
	const structuredContent = "# Introduction\n\nThis is the first paragraph.\n\n## Details\n\nMore content here."

	// flatContent has no headings and no blank lines — the content-shape
	// heuristic should downgrade HeadingAware to false even for structured sources.
	const flatContent = "A single continuous block of text with no paragraph breaks or heading markers at all."

	// chatContent simulates a short chat message.
	const chatContent = "hey, can you send me the report? thanks"

	tests := []struct {
		name            string
		sourceType      model.SourceType
		content         string
		wantHeadingAware bool
		wantTargetSize  int
		wantMaxSize     int
		wantOverlap     int
	}{
		// --- Long-form / structured sources (regression guard) ---
		{
			name:            "filesystem_structured_keeps_defaults",
			sourceType:      model.SourceFilesystem,
			content:         structuredContent,
			wantHeadingAware: true,
			wantTargetSize:  longFormTargetSize, // 2000 — must match scheduler default
			wantMaxSize:     longFormMaxSize,    // 4000
			wantOverlap:     longFormOverlap,   // 100
		},
		{
			name:            "notion_structured_keeps_defaults",
			sourceType:      model.SourceNotion,
			content:         structuredContent,
			wantHeadingAware: true,
			wantTargetSize:  longFormTargetSize,
			wantMaxSize:     longFormMaxSize,
			wantOverlap:     longFormOverlap,
		},
		{
			name:            "github_structured_keeps_defaults",
			sourceType:      model.SourceGitHub,
			content:         structuredContent,
			wantHeadingAware: true,
			wantTargetSize:  longFormTargetSize,
			wantMaxSize:     longFormMaxSize,
			wantOverlap:     longFormOverlap,
		},
		{
			name:            "gdrive_structured_keeps_defaults",
			sourceType:      model.SourceGDrive,
			content:         structuredContent,
			wantHeadingAware: true,
			wantTargetSize:  longFormTargetSize,
			wantMaxSize:     longFormMaxSize,
			wantOverlap:     longFormOverlap,
		},
		// --- Long-form source with flat content (content-shape override) ---
		{
			name:            "filesystem_flat_content_disables_heading_aware",
			sourceType:      model.SourceFilesystem,
			content:         flatContent,
			wantHeadingAware: false, // BI heuristic: no structure detected
			wantTargetSize:  longFormTargetSize,
			wantMaxSize:     longFormMaxSize,
			wantOverlap:     longFormOverlap,
		},
		// --- Short chat sources ---
		{
			name:            "slack_uses_small_chunks",
			sourceType:      model.SourceSlack,
			content:         chatContent,
			wantHeadingAware: false,
			wantTargetSize:  chatTargetSize, // 900
			wantMaxSize:     chatMaxSize,    // 1500
			wantOverlap:     chatOverlap,   // 80
		},
		{
			name:            "discord_uses_small_chunks",
			sourceType:      model.SourceDiscord,
			content:         chatContent,
			wantHeadingAware: false,
			wantTargetSize:  chatTargetSize,
			wantMaxSize:     chatMaxSize,
			wantOverlap:     chatOverlap,
		},
		{
			name:            "telegram_uses_small_chunks",
			sourceType:      model.SourceTelegram,
			content:         chatContent,
			wantHeadingAware: false,
			wantTargetSize:  chatTargetSize,
			wantMaxSize:     chatMaxSize,
			wantOverlap:     chatOverlap,
		},
		// --- Memory / agent sources ---
		{
			name:            "secretary_uses_mid_chunks",
			sourceType:      model.SourceSecretary,
			content:         "Session summary for 2024-01-15. Tasks completed: review PR #42.",
			wantHeadingAware: false,
			wantTargetSize:  memTargetSize, // 1200
			wantMaxSize:     memMaxSize,    // 2500
			wantOverlap:     memOverlap,   // 100
		},
		{
			name:            "llm_memory_uses_mid_chunks",
			sourceType:      model.SourceLLMMemory,
			content:         "The user prefers concise answers and works in Go.",
			wantHeadingAware: false,
			wantTargetSize:  memTargetSize,
			wantMaxSize:     memMaxSize,
			wantOverlap:     memOverlap,
		},
		// --- Unknown source type falls back to long-form defaults ---
		{
			name:            "unknown_source_falls_back_to_defaults",
			sourceType:      model.SourceType("unknown-future-source"),
			content:         structuredContent,
			wantHeadingAware: true,
			wantTargetSize:  longFormTargetSize,
			wantMaxSize:     longFormMaxSize,
			wantOverlap:     longFormOverlap,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			doc := model.Document{
				SourceType: tc.sourceType,
				Content:    tc.content,
			}
			got := SelectOptions(doc)

			if got.HeadingAware != tc.wantHeadingAware {
				t.Errorf("HeadingAware: got %v, want %v", got.HeadingAware, tc.wantHeadingAware)
			}
			if got.TargetSize != tc.wantTargetSize {
				t.Errorf("TargetSize: got %d, want %d", got.TargetSize, tc.wantTargetSize)
			}
			if got.MaxSize != tc.wantMaxSize {
				t.Errorf("MaxSize: got %d, want %d", got.MaxSize, tc.wantMaxSize)
			}
			if got.Overlap != tc.wantOverlap {
				t.Errorf("Overlap: got %d, want %d", got.Overlap, tc.wantOverlap)
			}
		})
	}
}

// TestSelectOptionsFilesystemRegressionSplit is an end-to-end regression test:
// a filesystem document with standard Markdown content must produce the same
// chunks it would have before issue #60 (HeadingAware=true, 2000/4000/100).
func TestSelectOptionsFilesystemRegressionSplit(t *testing.T) {
	t.Parallel()

	content := "# Header\n\nParagraph one.\n\n## Sub-header\n\nParagraph two."
	doc := model.Document{
		SourceType: model.SourceFilesystem,
		Content:    content,
	}

	opts := SelectOptions(doc)

	// Verify the options match the old hardcoded values exactly.
	want := Options{
		TargetSize:   2000,
		MaxSize:      4000,
		Overlap:      100,
		HeadingAware: true,
	}
	if opts != want {
		t.Errorf("regression: SelectOptions returned %+v, want %+v", opts, want)
	}

	// Also verify that Split produces output (sanity check).
	chunks := Split(content, opts)
	if len(chunks) == 0 {
		t.Error("Split returned no chunks for non-empty content")
	}
}
