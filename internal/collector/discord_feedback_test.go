package collector_test

import (
	"context"
	"testing"

	"github.com/baekenough/second-brain/internal/collector"
)

// mockFeedbackRecorder records calls to Record for assertion.
type mockFeedbackRecorder struct {
	calls []collector.FeedbackEntry
	err   error
}

func (m *mockFeedbackRecorder) Record(_ context.Context, entry collector.FeedbackEntry) (int64, error) {
	if m.err != nil {
		return 0, m.err
	}
	m.calls = append(m.calls, entry)
	return int64(len(m.calls)), nil
}

// Compile-time assertion: mockFeedbackRecorder satisfies FeedbackRecorder.
var _ collector.FeedbackRecorder = (*mockFeedbackRecorder)(nil)

const (
	testBotID     = "bot-999"
	testUserID    = "user-123"
	testChannelID = "chan-001"
	testMessageID = "msg-001"
	testGuildID   = "guild-001"
	testContent   = "this is the bot answer content"
)

// callProcessReactionFeedback is a thin helper that calls the exported pure
// function with common defaults.
func callProcessReactionFeedback(
	t *testing.T,
	botUserID, reactorUserID, msgAuthorID, emoji string,
	recorder collector.FeedbackRecorder,
) bool {
	t.Helper()
	return collector.ExportProcessReactionFeedback(
		context.Background(),
		botUserID,
		reactorUserID,
		msgAuthorID,
		emoji,
		testChannelID,
		testMessageID,
		testGuildID,
		testContent,
		recorder,
	)
}

// TestOnReactionAdd_BotOwnReactionIgnored verifies that a reaction added by the
// bot itself (the automatic 👍/👎 we add on send) is silently filtered.
func TestOnReactionAdd_BotOwnReactionIgnored(t *testing.T) {
	t.Parallel()

	rec := &mockFeedbackRecorder{}
	// reactorUserID == botUserID  → should be ignored
	got := callProcessReactionFeedback(t, testBotID, testBotID, testBotID, "👍", rec)

	if got {
		t.Fatal("expected false (bot self-reaction ignored), got true")
	}
	if len(rec.calls) != 0 {
		t.Fatalf("expected 0 Record calls for bot self-reaction, got %d", len(rec.calls))
	}
}

// TestOnReactionAdd_NonBotMessageIgnored verifies that reactions on messages NOT
// authored by the bot are silently ignored.
func TestOnReactionAdd_NonBotMessageIgnored(t *testing.T) {
	t.Parallel()

	rec := &mockFeedbackRecorder{}
	// msgAuthorID != botUserID → should be ignored
	got := callProcessReactionFeedback(t, testBotID, testUserID, "other-user-456", "👍", rec)

	if got {
		t.Fatal("expected false (non-bot message ignored), got true")
	}
	if len(rec.calls) != 0 {
		t.Fatalf("expected 0 Record calls for non-bot message, got %d", len(rec.calls))
	}
}

// TestOnReactionAdd_ThumbsUp_RecordsPositive verifies that a 👍 reaction on a bot
// message triggers a Record call with Thumbs=+1.
func TestOnReactionAdd_ThumbsUp_RecordsPositive(t *testing.T) {
	t.Parallel()

	rec := &mockFeedbackRecorder{}
	got := callProcessReactionFeedback(t, testBotID, testUserID, testBotID, "👍", rec)

	if !got {
		t.Fatal("expected true (feedback recorded), got false")
	}
	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 Record call, got %d", len(rec.calls))
	}

	entry := rec.calls[0]
	if entry.Thumbs != 1 {
		t.Errorf("Thumbs: want 1, got %d", entry.Thumbs)
	}
	if entry.Source != "discord_bot" {
		t.Errorf("Source: want %q, got %q", "discord_bot", entry.Source)
	}
	if entry.UserID == nil || *entry.UserID != testUserID {
		t.Errorf("UserID: want %q, got %v", testUserID, entry.UserID)
	}
	if entry.SessionID == nil || *entry.SessionID != testMessageID {
		t.Errorf("SessionID: want %q, got %v", testMessageID, entry.SessionID)
	}
	if entry.Comment == nil || *entry.Comment != testContent {
		t.Errorf("Comment: want %q, got %v", testContent, entry.Comment)
	}
	if entry.Metadata["emoji"] != "👍" {
		t.Errorf("Metadata[emoji]: want %q, got %v", "👍", entry.Metadata["emoji"])
	}
}

// TestOnReactionAdd_ThumbsDown_RecordsNegative verifies that a 👎 reaction on a
// bot message triggers a Record call with Thumbs=-1.
func TestOnReactionAdd_ThumbsDown_RecordsNegative(t *testing.T) {
	t.Parallel()

	rec := &mockFeedbackRecorder{}
	got := callProcessReactionFeedback(t, testBotID, testUserID, testBotID, "👎", rec)

	if !got {
		t.Fatal("expected true (feedback recorded), got false")
	}
	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 Record call, got %d", len(rec.calls))
	}
	if rec.calls[0].Thumbs != -1 {
		t.Errorf("Thumbs: want -1, got %d", rec.calls[0].Thumbs)
	}
}

// TestOnReactionAdd_OtherEmoji_Ignored verifies that emojis other than 👍/👎
// are silently filtered (no Record call).
func TestOnReactionAdd_OtherEmoji_Ignored(t *testing.T) {
	t.Parallel()

	otherEmojis := []string{"💯", "❤️", "🔥", "✅", "😀"}
	for _, emoji := range otherEmojis {
		emoji := emoji
		t.Run(emoji, func(t *testing.T) {
			t.Parallel()
			rec := &mockFeedbackRecorder{}
			got := callProcessReactionFeedback(t, testBotID, testUserID, testBotID, emoji, rec)
			if got {
				t.Errorf("emoji %q: expected false (other emoji ignored), got true", emoji)
			}
			if len(rec.calls) != 0 {
				t.Errorf("emoji %q: expected 0 Record calls, got %d", emoji, len(rec.calls))
			}
		})
	}
}

// TestOnReactionAdd_NilFeedbackStore_NoOp verifies that passing a nil recorder
// (no store injected) does not panic and returns false.
func TestOnReactionAdd_NilFeedbackStore_NoOp(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("processReactionFeedback panicked with nil recorder: %v", r)
		}
	}()

	got := callProcessReactionFeedback(t, testBotID, testUserID, testBotID, "👍", nil)
	if got {
		t.Fatal("expected false when recorder is nil, got true")
	}
}

// TestOnReactionAdd_Metadata_ContainsIDs verifies that the Metadata map includes
// all required fields for observability and debugging.
func TestOnReactionAdd_Metadata_ContainsIDs(t *testing.T) {
	t.Parallel()

	rec := &mockFeedbackRecorder{}
	collector.ExportProcessReactionFeedback(
		context.Background(),
		testBotID,
		testUserID,
		testBotID,
		"👍",
		testChannelID,
		testMessageID,
		testGuildID,
		testContent,
		rec,
	)

	if len(rec.calls) == 0 {
		t.Fatal("expected at least one Record call")
	}

	meta := rec.calls[0].Metadata
	checks := map[string]string{
		"guild_id":   testGuildID,
		"channel_id": testChannelID,
		"message_id": testMessageID,
		"emoji":      "👍",
	}
	for key, want := range checks {
		got, ok := meta[key]
		if !ok {
			t.Errorf("Metadata missing key %q", key)
			continue
		}
		if got != want {
			t.Errorf("Metadata[%q]: want %q, got %v", key, want, got)
		}
	}
}
