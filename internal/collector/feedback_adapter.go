package collector

import (
	"context"
	"fmt"

	"github.com/baekenough/second-brain/internal/store"
)

// feedbackStoreAdapter wraps store.FeedbackStore to satisfy the FeedbackRecorder
// interface. It translates between the collector-local FeedbackEntry type and the
// store.Feedback type so the collector package remains decoupled from the store
// package's concrete type.
//
// The adapter delegates to store.FeedbackStore.Upsert so that a user clicking 👍
// after 👎 (or vice-versa) replaces the previous reaction rather than creating a
// duplicate row. When UserID or SessionID is nil the store falls through to Record.
type feedbackStoreAdapter struct {
	store *store.FeedbackStore
}

// NewFeedbackStoreAdapter wraps the given store.FeedbackStore as a FeedbackRecorder.
// Pass the result to DiscordGateway.SetFeedbackStore.
func NewFeedbackStoreAdapter(s *store.FeedbackStore) FeedbackRecorder {
	return &feedbackStoreAdapter{store: s}
}

// Record translates entry into a store.Feedback and delegates to
// store.FeedbackStore.Upsert, which handles the toggle/replace semantics.
func (a *feedbackStoreAdapter) Record(ctx context.Context, entry FeedbackEntry) (int64, error) {
	id, err := a.store.Upsert(ctx, store.Feedback{
		Source:    entry.Source,
		SessionID: entry.SessionID,
		UserID:    entry.UserID,
		Thumbs:    entry.Thumbs,
		Comment:   entry.Comment,
		Metadata:  entry.Metadata,
	})
	if err != nil {
		return 0, fmt.Errorf("feedback adapter upsert: %w", err)
	}
	return id, nil
}
