package briefing

import (
	"context"
	"errors"
	"log/slog"

	"github.com/baekenough/second-brain/internal/llm"
)

// Generate produces a briefing for in.
//
// It never returns an error for an LLM failure. A briefing is an aid on top of
// the action list; if the model is down, slow, disabled, or returns something
// unusable, the caller still gets a usable Result — the deterministic
// count-only fallback, flagged Degraded so the UI can say so. Propagating the
// error instead would let a summarisation failure take down the page whose data
// is already in hand.
//
// A truncated reply (llm.ErrTruncated: generation stopped on the token budget)
// is treated as a failure, not as content. Its JSON is incomplete by
// definition, and salvaging a partial answer is precisely how a sentence with
// no evidence behind it reaches the user.
//
// An empty action set never reaches the model: asking for a summary of nothing
// reliably produces invention.
func Generate(ctx context.Context, c llm.Completer, in Input) (Result, error) {
	if len(in.Actions) == 0 {
		return fallbackResult(in), nil
	}
	if c == nil || !c.Enabled() {
		slog.Info("briefing: llm unavailable, returning aggregate only",
			"action_count", len(in.Actions), "reason", "disabled")
		return fallbackResult(in), nil
	}

	raw, err := c.CompleteWithMessages(ctx, systemPrompt, []llm.Message{
		{Role: "user", Content: buildUserMessage(in)},
	})
	if err != nil {
		// Only a closed-vocabulary reason is logged. Neither the prompt (which
		// contains action summaries) nor the reply (which restates the user's
		// conversations) may appear in logs.
		reason := "error"
		if errors.Is(err, llm.ErrTruncated) {
			reason = "truncated"
		} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			reason = "timeout"
		}
		slog.Warn("briefing: llm call failed, returning aggregate only",
			"action_count", len(in.Actions), "reason", reason)
		return fallbackResult(in), nil
	}

	result, err := Validate(raw, in)
	if err != nil {
		// err here is already content-free by construction (decodePayload builds
		// it from lengths and offsets only), so it is safe to log as-is.
		slog.Warn("briefing: reply could not be decoded, returning aggregate only",
			"action_count", len(in.Actions), "error", err)
	}
	return result, nil
}
