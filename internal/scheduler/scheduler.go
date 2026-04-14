// Package scheduler periodically triggers collectors and persists the
// resulting documents.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/baekenough/second-brain/internal/collector"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/search"
)

// defaultMaxEmbedChars is the default character limit for text passed to the
// embedding API. Long documents are truncated at this boundary to avoid token
// limit errors. Override with MAX_EMBED_CHARS env var.
//
// TODO(Phase 1): replace with chunk-based embedding using chunks table.
const defaultMaxEmbedChars = 8000

// maxEmbedChars returns the configured character limit for embedding text.
// It reads MAX_EMBED_CHARS from the environment; falls back to defaultMaxEmbedChars.
func maxEmbedChars() int {
	if v := os.Getenv("MAX_EMBED_CHARS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxEmbedChars
}

// DocumentUpserter is the subset of the document store used by the scheduler.
type DocumentUpserter interface {
	Upsert(ctx context.Context, doc *model.Document) error
	LastCollectedAt(ctx context.Context, src model.SourceType, fallback time.Time) time.Time
	RecordCollectionLog(ctx context.Context, src model.SourceType, started time.Time, count int, err error) error
	MarkDeleted(ctx context.Context, sourceType model.SourceType, activeIDs []string) (int, error)
}

// Scheduler wraps robfig/cron and manages periodic collection runs.
type Scheduler struct {
	cron       *cron.Cron
	collectors []collector.Collector
	store      DocumentUpserter
	embed      *search.EmbedClient

	// running is set to 1 while a collection cycle is in progress.
	// CompareAndSwap from 0→1 acts as a non-blocking try-lock so that
	// overlapping cron ticks and manual triggers are skipped rather than
	// piling up.
	running atomic.Bool
}

// New returns a Scheduler with the given collectors and storage backend.
func New(store DocumentUpserter, embed *search.EmbedClient, collectors ...collector.Collector) *Scheduler {
	c := cron.New(cron.WithSeconds())
	return &Scheduler{
		cron:       c,
		collectors: collectors,
		store:      store,
		embed:      embed,
	}
}

// Register adds a cron job for each enabled collector using the given interval
// (e.g. "@every 1h").
func (s *Scheduler) Register(interval time.Duration) error {
	spec := fmt.Sprintf("@every %s", interval)
	for _, col := range s.collectors {
		if !col.Enabled() {
			slog.Info("scheduler: collector disabled, skipping", "name", col.Name())
			continue
		}
		c := col // capture loop variable
		if _, err := s.cron.AddFunc(spec, func() {
			s.run(context.Background(), c)
		}); err != nil {
			return fmt.Errorf("register collector %s: %w", c.Name(), err)
		}
		slog.Info("scheduler: registered collector", "name", c.Name(), "interval", interval)
	}
	return nil
}

// Start begins the cron scheduler. It is non-blocking.
func (s *Scheduler) Start() { s.cron.Start() }

// Stop gracefully halts the scheduler and waits for running jobs to finish.
func (s *Scheduler) Stop() { s.cron.Stop() }

// TriggerAll runs all enabled collectors immediately in the background.
// It is intended for manual /collect/trigger API calls.
// If a collection is already in progress the call is a no-op and a warning
// is logged.
func (s *Scheduler) TriggerAll(ctx context.Context) {
	if !s.running.CompareAndSwap(false, true) {
		slog.Warn("scheduler: collection already running, skipping trigger")
		return
	}
	go func() {
		defer s.running.Store(false)
		for _, col := range s.collectors {
			if !col.Enabled() {
				continue
			}
			s.runCollector(ctx, col)
		}
	}()
}

// run is the cron-tick entry point. It acquires the running flag and
// delegates to runCollector for each enabled collector.
func (s *Scheduler) run(ctx context.Context, col collector.Collector) {
	if !s.running.CompareAndSwap(false, true) {
		slog.Warn("scheduler: collection already running, skipping trigger",
			"collector", col.Name())
		return
	}
	defer s.running.Store(false)
	s.runCollector(ctx, col)
}

// runCollector executes a single collection cycle for one collector.
// It must only be called while s.running is held (i.e. set to true).
func (s *Scheduler) runCollector(ctx context.Context, col collector.Collector) {
	started := time.Now()
	defaultSince := time.Time{} // zero time = collect all files on first run
	since := s.store.LastCollectedAt(ctx, col.Source(), defaultSince)

	slog.Info("scheduler: starting collection",
		"collector", col.Name(),
		"since", since.Format(time.RFC3339),
	)

	docs, err := col.Collect(ctx, since)
	if err != nil {
		slog.Error("scheduler: collection failed",
			"collector", col.Name(), "error", err)
		_ = s.store.RecordCollectionLog(ctx, col.Source(), started, 0, err)
		return
	}

	// Optionally enrich documents with embeddings before upserting.
	if s.embed.Enabled() && len(docs) > 0 {
		s.embedDocuments(ctx, docs)
	}

	count := 0
	for i := range docs {
		if err := s.store.Upsert(ctx, &docs[i]); err != nil {
			slog.Warn("scheduler: upsert failed",
				"collector", col.Name(),
				"source_id", docs[i].SourceID,
				"error", err)
			continue
		}
		count++
	}

	_ = s.store.RecordCollectionLog(ctx, col.Source(), started, count, nil)
	slog.Info("scheduler: collection complete",
		"collector", col.Name(),
		"upserted", count,
		"total", len(docs),
		"elapsed", time.Since(started).Round(time.Millisecond),
	)

	// Soft-delete detection: if the collector can enumerate all current source IDs,
	// mark any DB-active documents whose source IDs are no longer present.
	if dd, ok := col.(collector.DeletionDetector); ok {
		allIDs, err := dd.ListActiveSourceIDs(ctx)
		if err != nil {
			slog.Warn("scheduler: deletion detection ID listing failed",
				"collector", col.Name(), "error", err)
			return
		}
		deleted, err := s.store.MarkDeleted(ctx, col.Source(), allIDs)
		if err != nil {
			slog.Warn("scheduler: deletion detection failed",
				"collector", col.Name(), "error", err)
			return
		}
		if deleted > 0 {
			slog.Info("scheduler: marked deleted",
				"collector", col.Name(), "count", deleted)
		}
	}
}

// embedDocuments fills the Embedding field of each document by calling the
// embedding API in chunks to avoid timeout and payload-too-large errors.
func (s *Scheduler) embedDocuments(ctx context.Context, docs []model.Document) {
	const batchSize = 20
	maxLen := maxEmbedChars()

	for start := 0; start < len(docs); start += batchSize {
		end := start + batchSize
		if end > len(docs) {
			end = len(docs)
		}

		chunk := docs[start:end]
		texts := make([]string, len(chunk))
		for i, d := range chunk {
			// Use title + content for a richer embedding context.
			// Limit text length to avoid token limits.
			// TODO(Phase 1): replace with chunk-based embedding using chunks table.
			text := d.Title + "\n\n" + d.Content
			if len(text) > maxLen {
				origLen := len(text)
				text = text[:maxLen]
				slog.Warn("scheduler: content truncated for embedding",
					"source_id", d.SourceID,
					"original_len", origLen,
					"truncated_to", maxLen,
					"lost_chars", origLen-maxLen,
				)
			}
			texts[i] = text
		}

		vecs, err := s.embed.EmbedBatch(ctx, texts)
		if err != nil {
			slog.Warn("scheduler: batch embedding chunk failed, skipping chunk",
				"error", err, "start", start, "end", end)
			continue
		}
		for i := range chunk {
			if i < len(vecs) {
				docs[start+i].Embedding = vecs[i]
			}
		}

		slog.Info("scheduler: embedded chunk", "start", start, "end", end, "total", len(docs))
	}
}

// Collectors returns the list of registered collectors (for status reporting).
func (s *Scheduler) Collectors() []collector.Collector { return s.collectors }

// slackCollector returns the first *collector.SlackCollector in the registry,
// or nil if none is registered.
func (s *Scheduler) slackCollector() *collector.SlackCollector {
	for _, col := range s.collectors {
		if sc, ok := col.(*collector.SlackCollector); ok {
			return sc
		}
	}
	return nil
}

// LookupSlackChannel resolves a channel name (case-insensitive, "#" stripped)
// to its Slack channel ID by querying the channels the bot is a member of.
// Returns ErrSlackCollectorNotFound when no Slack collector is configured,
// ErrSlackChannelNotFound when the name does not match any member channel.
func (s *Scheduler) LookupSlackChannel(ctx context.Context, name string) (id, channelName string, err error) {
	sc := s.slackCollector()
	if sc == nil {
		return "", "", ErrSlackCollectorNotFound
	}
	id, channelName, found, err := sc.FindMemberChannelByName(ctx, name)
	if err != nil {
		return "", "", fmt.Errorf("lookup slack channel %q: %w", name, err)
	}
	if !found {
		return "", "", ErrSlackChannelNotFound
	}
	return id, channelName, nil
}

// ForceCollectSlackChannel runs a full-history collection (since = zero time)
// for a single Slack channel and persists the resulting documents.
// It bypasses the source-level LastCollectedAt and is intended for manual
// POST /api/v1/collect/slack/channel calls.
//
// If channelID is empty, channelName is used to resolve the ID via the Slack
// API (the bot must be a member of the channel).
//
// Returns the number of upserted documents and any error.
func (s *Scheduler) ForceCollectSlackChannel(ctx context.Context, channelID, channelName string) (int, error) {
	sc := s.slackCollector()
	if sc == nil {
		return 0, ErrSlackCollectorNotFound
	}
	if !sc.Enabled() {
		return 0, fmt.Errorf("slack collector is disabled")
	}

	// Resolve channel ID from name when not provided.
	if channelID == "" {
		id, resolvedName, found, err := sc.FindMemberChannelByName(ctx, channelName)
		if err != nil {
			return 0, fmt.Errorf("resolve channel name %q: %w", channelName, err)
		}
		if !found {
			return 0, ErrSlackChannelNotFound
		}
		channelID = id
		channelName = resolvedName
	}

	started := time.Now()
	slog.Info("scheduler: force-collecting slack channel",
		"channel_id", channelID,
		"channel_name", channelName,
	)

	docs, err := sc.CollectChannel(ctx, channelID, channelName, time.Time{})
	if err != nil {
		slog.Error("scheduler: force-collect failed",
			"channel_id", channelID, "channel_name", channelName, "error", err)
		_ = s.store.RecordCollectionLog(ctx, sc.Source(), started, 0, err)
		return 0, fmt.Errorf("collect channel %s: %w", channelID, err)
	}

	if s.embed.Enabled() && len(docs) > 0 {
		s.embedDocuments(ctx, docs)
	}

	count := 0
	for i := range docs {
		if err := s.store.Upsert(ctx, &docs[i]); err != nil {
			slog.Warn("scheduler: force-collect upsert failed",
				"channel_id", channelID,
				"source_id", docs[i].SourceID,
				"error", err)
			continue
		}
		count++
	}

	_ = s.store.RecordCollectionLog(ctx, sc.Source(), started, count, nil)
	slog.Info("scheduler: force-collect complete",
		"channel_id", channelID,
		"channel_name", channelName,
		"upserted", count,
		"total", len(docs),
		"elapsed", time.Since(started).Round(time.Millisecond),
	)
	return count, nil
}

// Sentinel errors for Slack channel operations.
var (
	ErrSlackCollectorNotFound = fmt.Errorf("slack collector not configured")
	ErrSlackChannelNotFound   = fmt.Errorf("channel not found in bot member list")
)
