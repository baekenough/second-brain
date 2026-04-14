package collector

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
)

// watcherStore is the subset of the document store required by SlackChannelWatcher.
type watcherStore interface {
	Upsert(ctx context.Context, doc *model.Document) error
	RecordCollectionLog(ctx context.Context, src model.SourceType, started time.Time, count int, err error) error
}

// SlackChannelWatcher periodically polls the list of channels the bot belongs
// to and triggers an immediate first-collection for any newly discovered
// channel, rather than waiting for the next cron cycle.
type SlackChannelWatcher struct {
	collector *SlackCollector
	store     watcherStore
	embed     *search.EmbedClient

	mu       sync.Mutex
	seen     map[string]struct{}
	interval time.Duration
}

// NewSlackChannelWatcher creates a watcher that polls on the given interval.
func NewSlackChannelWatcher(
	col *SlackCollector,
	store watcherStore,
	embed *search.EmbedClient,
	interval time.Duration,
) *SlackChannelWatcher {
	return &SlackChannelWatcher{
		collector: col,
		store:     store,
		embed:     embed,
		seen:      make(map[string]struct{}),
		interval:  interval,
	}
}

// Run starts the polling loop and blocks until ctx is cancelled.
// On the first tick, all current channels are registered in the seen set
// without triggering collection (the cron scheduler handles the initial run).
// On subsequent ticks, any channel not yet in the seen set is collected
// immediately from the beginning of history (since=zero time).
func (w *SlackChannelWatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	firstTick := true
	for {
		select {
		case <-ctx.Done():
			slog.Info("slack watcher: stopped")
			return
		case <-ticker.C:
			if firstTick {
				w.seedSeen(ctx)
				firstTick = false
				continue
			}
			w.checkNewChannels(ctx)
		}
	}
}

// seedSeen populates the seen set with all current member channels on startup.
// No collection is triggered — this prevents a mass re-collection on restart.
func (w *SlackChannelWatcher) seedSeen(ctx context.Context) {
	channels, err := w.collector.ListMemberChannels(ctx)
	if err != nil {
		slog.Warn("slack watcher: failed to seed channel list", "error", err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ch := range channels {
		w.seen[ch.ID] = struct{}{}
	}
	slog.Info("slack watcher: seeded known channels", "count", len(channels))
}

// checkNewChannels lists current member channels and collects any that have
// not been seen before (i.e. the bot was just invited).
func (w *SlackChannelWatcher) checkNewChannels(ctx context.Context) {
	channels, err := w.collector.ListMemberChannels(ctx)
	if err != nil {
		slog.Warn("slack watcher: failed to list channels", "error", err)
		return
	}

	w.mu.Lock()
	var newChannels []slackChannel
	for _, ch := range channels {
		if _, ok := w.seen[ch.ID]; !ok {
			newChannels = append(newChannels, ch)
		}
	}
	w.mu.Unlock()

	for _, ch := range newChannels {
		slog.Info("slack watcher: new channel detected, collecting", "channel", ch.Name, "id", ch.ID)
		w.collectNewChannel(ctx, ch)

		w.mu.Lock()
		w.seen[ch.ID] = struct{}{}
		w.mu.Unlock()
	}
}

// collectNewChannel fetches the full history of a newly joined channel,
// optionally embeds documents, and upserts them into the store.
func (w *SlackChannelWatcher) collectNewChannel(ctx context.Context, ch slackChannel) {
	started := time.Now()

	// Collect full history (since=zero time).
	docs, err := w.collector.CollectChannel(ctx, ch.ID, ch.Name, time.Time{})
	if err != nil {
		slog.Error("slack watcher: collection failed",
			"channel", ch.Name, "error", err)
		_ = w.store.RecordCollectionLog(ctx, model.SourceSlack, started, 0, err)
		return
	}

	// Embed documents inline if embedding is enabled.
	if w.embed.Enabled() && len(docs) > 0 {
		w.embedDocuments(ctx, docs)
	}

	count := 0
	for i := range docs {
		if err := w.store.Upsert(ctx, &docs[i]); err != nil {
			slog.Warn("slack watcher: upsert failed",
				"channel", ch.Name,
				"source_id", docs[i].SourceID,
				"error", err)
			continue
		}
		count++
	}

	_ = w.store.RecordCollectionLog(ctx, model.SourceSlack, started, count, nil)
	slog.Info("slack watcher: new channel collected",
		"channel", ch.Name,
		"upserted", count,
		"total", len(docs),
		"elapsed", time.Since(started).Round(time.Millisecond),
	)
}

// embedDocuments fills the Embedding field of each document by calling the
// embedding API in batches. Long documents are truncated to avoid token limits.
func (w *SlackChannelWatcher) embedDocuments(ctx context.Context, docs []model.Document) {
	const batchSize = 20
	maxLen := watcherMaxEmbedChars()

	for start := 0; start < len(docs); start += batchSize {
		end := start + batchSize
		if end > len(docs) {
			end = len(docs)
		}

		chunk := docs[start:end]
		texts := make([]string, len(chunk))
		for i, d := range chunk {
			text := d.Title + "\n\n" + d.Content
			if len(text) > maxLen {
				origLen := len(text)
				text = text[:maxLen]
				slog.Warn("slack watcher: content truncated for embedding",
					"source_id", d.SourceID,
					"original_len", origLen,
					"truncated_to", maxLen,
				)
			}
			texts[i] = text
		}

		vecs, err := w.embed.EmbedBatch(ctx, texts)
		if err != nil {
			slog.Warn("slack watcher: batch embedding failed, skipping chunk",
				"error", err, "start", start, "end", end)
			continue
		}
		for i := range chunk {
			if i < len(vecs) {
				docs[start+i].Embedding = vecs[i]
			}
		}
	}
}

// watcherMaxEmbedChars returns the character limit for embedding text,
// reading MAX_EMBED_CHARS from the environment with a default of 8000.
func watcherMaxEmbedChars() int {
	if v := os.Getenv("MAX_EMBED_CHARS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 8000
}
