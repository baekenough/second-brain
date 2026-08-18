package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/baekenough/second-brain/internal/action"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ThreadLatestMessage is one row of StructuralSignalLister.ListLatestPerThread's
// result: the most recent document in a conversation thread, plus what's
// needed to determine whether it is an "awaiting my reply" candidate (spec
// §7.1) and to describe it on an action card.
//
// Title and EventAt exist only for the card: the summary this worker writes
// used to be a fixed constant, which made every awaiting_my_reply row read
// identically. They are the two document columns that carry conversation
// context without carrying the message body.
type ThreadLatestMessage struct {
	DocumentID uuid.UUID
	SourceType string
	Metadata   map[string]any
	Title      string
	// EventAt is COALESCE(occurred_at, collected_at) — occurred_at is
	// nullable, and ingest order is a serviceable stand-in for event time
	// when the collector could not determine one.
	EventAt time.Time
}

// StructuralSignalLister provides the thread-grouped SQL query consumed by
// StructuralSignalWorker (PgStructuralSignalLister satisfies this).
type StructuralSignalLister interface {
	ListLatestPerThread(ctx context.Context, limit int) ([]ThreadLatestMessage, error)
}

// ActionWriter is defined in extraction_worker.go (Task 11) and reused here
// unchanged — both workers write the same actions table via the same two
// methods.

// PgStructuralSignalLister is the pgx-backed implementation of
// StructuralSignalLister. The SQL lives entirely in this file (rather than
// internal/store) per this task's scope: it talks to the documents table
// directly via a *pgxpool.Pool.
type PgStructuralSignalLister struct {
	pool *pgxpool.Pool
}

// NewPgStructuralSignalLister returns a PgStructuralSignalLister backed by
// pool (typically (*store.Postgres).Pool()).
func NewPgStructuralSignalLister(pool *pgxpool.Pool) *PgStructuralSignalLister {
	return &PgStructuralSignalLister{pool: pool}
}

// listLatestPerThreadQuery groups gmail/sms/call-log/call-transcript
// documents into threads and returns only the single most recent document
// per thread (spec §7.1: "각 스레드에서 occurred_at 최대인 문서"). gmail
// threads are grouped by metadata->>'thread_id'; sms by
// metadata->>'contact_name'; call-log AND call-transcript are grouped
// TOGETHER by metadata->>'contact_name' (spec §7.1 table: both map to the
// same "call:" thread_key prefix, so a call and its transcript are one
// thread). sms documents flagged is_auth_like are excluded at the source
// (spec §7.4) — StructuralSignalWorker also re-checks this defensively.
// No LLM call — pure SQL, safe to run far more often than ExtractionWorker.
const listLatestPerThreadQuery = `
	WITH ranked AS (
		SELECT id, source_type, metadata, title,
		       COALESCE(occurred_at, collected_at) AS event_at,
		       row_number() OVER (
		           PARTITION BY
		               CASE
		                   WHEN source_type = 'gmail' THEN 'gmail:' || (metadata->>'thread_id')
		                   WHEN source_type = 'sms'   THEN 'sms:'   || COALESCE(metadata->>'contact_name', '')
		                   ELSE 'call:' || COALESCE(metadata->>'contact_name', '')
		               END
		           ORDER BY occurred_at DESC
		       ) AS rn
		FROM documents
		WHERE status = 'active'
		  AND source_type IN ('gmail', 'sms', 'call-log', 'call-transcript')
		  AND occurred_at >= now() - interval '30 days'
		  AND (source_type <> 'sms' OR COALESCE((metadata->>'is_auth_like')::boolean, false) = false)
	)
	SELECT id, source_type, metadata, title, event_at FROM ranked WHERE rn = 1
	LIMIT $1`

// ListLatestPerThread implements StructuralSignalLister.
func (l *PgStructuralSignalLister) ListLatestPerThread(ctx context.Context, limit int) ([]ThreadLatestMessage, error) {
	rows, err := l.pool.Query(ctx, listLatestPerThreadQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("list latest per thread: %w", err)
	}
	defer rows.Close()

	var out []ThreadLatestMessage
	for rows.Next() {
		var m ThreadLatestMessage
		var metaJSON []byte
		if err := rows.Scan(&m.DocumentID, &m.SourceType, &metaJSON, &m.Title, &m.EventAt); err != nil {
			return nil, fmt.Errorf("scan thread latest message: %w", err)
		}
		if len(metaJSON) > 0 {
			if err := json.Unmarshal(metaJSON, &m.Metadata); err != nil {
				return nil, fmt.Errorf("unmarshal thread latest message metadata: %w", err)
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// StructuralSignalWorkerConfig holds configuration for StructuralSignalWorker.
type StructuralSignalWorkerConfig struct {
	// Store lists the latest document per active thread. Typically
	// *PgStructuralSignalLister.
	Store StructuralSignalLister
	// Actions is the write side of the actions table (reused from Task 11).
	Actions ActionWriter
	// UserAddresses is config.UserEmailAddresses — used to infer gmail
	// direction, since gmail documents carry no "direction" metadata key.
	UserAddresses []string
	// Interval controls how often the worker polls. Defaults to 10 minutes.
	Interval time.Duration
	// BatchSize is the number of threads inspected per tick. Defaults to 500
	// (this is cheap SQL-only work, so the default batch is generous).
	BatchSize int
	// Now supplies the current instant used to age each thread ("3일째
	// 미응답"). Defaults to time.Now. Injectable so a test can assert the
	// rendered card without racing the wall clock.
	Now func() time.Time
}

// StructuralSignalWorker computes the "awaiting my reply" candidate action
// (spec §7.1) — the ONLY structural signal this plan implements; §7.2's
// merge display and §7.3's briefing generation are Part C, out of scope.
// It makes NO LLM call, so it can and should run more often than
// ExtractionWorker; a short default interval reflects that.
type StructuralSignalWorker struct {
	store         StructuralSignalLister
	actions       ActionWriter
	userAddresses []string
	interval      time.Duration
	batchSize     int
	now           func() time.Time
}

// NewStructuralSignalWorker constructs a StructuralSignalWorker from cfg.
// Panics when required fields are nil.
func NewStructuralSignalWorker(cfg StructuralSignalWorkerConfig) *StructuralSignalWorker {
	if cfg.Store == nil {
		panic("StructuralSignalWorkerConfig.Store must not be nil")
	}
	if cfg.Actions == nil {
		panic("StructuralSignalWorkerConfig.Actions must not be nil")
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 500
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &StructuralSignalWorker{
		store:         cfg.Store,
		actions:       cfg.Actions,
		userAddresses: cfg.UserAddresses,
		interval:      interval,
		batchSize:     batchSize,
		now:           now,
	}
}

// Run blocks until ctx is cancelled. Unlike ExtractionWorker, there is no
// LLM.Enabled() gate — this worker is pure SQL and always runs.
func (w *StructuralSignalWorker) Run(ctx context.Context) {
	slog.Info("structural signal worker started", "interval", w.interval, "batch_size", w.batchSize)
	w.Tick(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("structural signal worker stopped")
			return
		case <-ticker.C:
			w.Tick(ctx)
		}
	}
}

// tickOutcome is the single bucket one listed message falls into. Every
// early return in processMessage must name one: the buckets are what make a
// tick's work visible, and an unnamed return is invisible by construction.
type tickOutcome int

const (
	outcomeWritten tickOutcome = iota
	outcomeSkippedAuthLike
	outcomeSkippedOutbound
	outcomeSkippedNoCounterpart
	outcomeSkippedNoThreadKey
	outcomeFailed
	outcomeStatusFailed
)

// tickStats accounts for one Tick pass.
//
// It exists because this worker had exactly two observable states — "started"
// and "errored" — and a tick that wrote 419 actions logged the same thing as
// a tick that silently skipped all 419: nothing. When the actions table was
// found empty after a deploy, no log could say whether the worker had run,
// had run and skipped everything, or had never listed a row; the question was
// only settleable by dating rows against the actions_id_seq sequence. That
// blind spot, not the write path, is what turned a nine-minute wait for the
// next tick into a production DELETE of 447 rows.
//
// Counts only. A message's contents, its document id, its counterpart and its
// metadata values are all personal data and none of them belong in an
// operational log line that exists to answer "did the worker do its job?".
type tickStats struct {
	// ListFailed reports that the read failed, so no message was ever seen.
	// Distinct from Listed == 0, which means the query legitimately returned
	// nothing — identical symptoms, opposite causes.
	ListFailed           bool
	Listed               int
	Written              int
	SkippedAuthLike      int
	SkippedOutbound      int
	SkippedNoCounterpart int
	SkippedNoThreadKey   int
	Failed               int
	// StatusFailed counts actions that were written but left without an
	// 'open' action_status row. Those rows exist in the table and yet never
	// reach the /actions screen, which joins action_status — a full table
	// behind an empty screen.
	StatusFailed int
}

// accountedFor totals the outcome buckets. It must equal Listed; a shortfall
// means some path returns without being counted, which is the exact defect
// tickStats was introduced to prevent from recurring. StatusFailed is not
// added: those messages are already counted in Written (the action row was
// persisted), and StatusFailed is a second, narrower fact about them.
func (s tickStats) accountedFor() int {
	return s.Written + s.SkippedAuthLike + s.SkippedOutbound +
		s.SkippedNoCounterpart + s.SkippedNoThreadKey + s.Failed
}

func (s *tickStats) record(o tickOutcome) {
	switch o {
	case outcomeWritten:
		s.Written++
	case outcomeSkippedAuthLike:
		s.SkippedAuthLike++
	case outcomeSkippedOutbound:
		s.SkippedOutbound++
	case outcomeSkippedNoCounterpart:
		s.SkippedNoCounterpart++
	case outcomeSkippedNoThreadKey:
		s.SkippedNoThreadKey++
	case outcomeFailed:
		s.Failed++
	case outcomeStatusFailed:
		s.Written++
		s.StatusFailed++
	}
}

// Tick is exported (unlike ExtractionWorker's unexported tick) so tests can
// drive one pass deterministically without a ticker. It returns what the pass
// did so a test can assert the accounting the log line reports.
func (w *StructuralSignalWorker) Tick(ctx context.Context) tickStats {
	var stats tickStats
	messages, err := w.store.ListLatestPerThread(ctx, w.batchSize)
	if err != nil {
		stats.ListFailed = true
		slog.Warn("structural signal worker: list failed", "error", err)
		return stats
	}
	stats.Listed = len(messages)
	for _, m := range messages {
		stats.record(w.processMessage(ctx, m))
	}

	// Logged at INFO on every tick, including the all-zero one: the absence of
	// work is the observation that was missing, so it cannot be the case that
	// the quiet tick is also the silent one.
	slog.Info("structural signal worker: tick complete",
		"listed", stats.Listed,
		"written", stats.Written,
		"failed", stats.Failed,
		"status_failed", stats.StatusFailed,
		"skipped_auth_like", stats.SkippedAuthLike,
		"skipped_outbound", stats.SkippedOutbound,
		"skipped_no_counterpart", stats.SkippedNoCounterpart,
		"skipped_no_thread_key", stats.SkippedNoThreadKey,
	)
	return stats
}

func (w *StructuralSignalWorker) processMessage(ctx context.Context, m ThreadLatestMessage) tickOutcome {
	doc := &model.Document{
		ID:         m.DocumentID,
		SourceType: model.SourceType(m.SourceType),
		Metadata:   m.Metadata,
		Title:      m.Title,
	}
	if !m.EventAt.IsZero() {
		eventAt := m.EventAt
		doc.OccurredAt = &eventAt
	}

	// Defense in depth: ListLatestPerThread's SQL already excludes
	// is_auth_like sms documents at the source (spec §7.4), but this Go-level
	// check protects any future caller of StructuralSignalLister that does
	// not pre-filter.
	if authLike, _ := doc.Metadata["is_auth_like"].(bool); authLike {
		return outcomeSkippedAuthLike
	}

	if isOutbound(doc) {
		return outcomeSkippedOutbound
	}

	counterpart, ok := action.CounterpartIdentity(doc, func(addr string) bool { return action.IsUserAddress(w.userAddresses, addr) })
	if !ok {
		return outcomeSkippedNoCounterpart
	}

	threadKey, ok := threadKeyForDocument(doc)
	if !ok {
		return outcomeSkippedNoThreadKey
	}

	identityKey := action.BuildIdentityKey(threadKey, string(model.KindAwaitingMyReply), counterpart, "")

	// displayName is the label a human reads; counterpart (above) is the
	// hash input. They are intentionally different strings — see
	// action.CounterpartDisplay. An unresolvable display name leaves both the
	// card label and counterpart_entity_id empty rather than inventing one.
	displayName, entityType, hasDisplay := action.CounterpartDisplay(doc)
	if !hasDisplay {
		displayName, entityType = "", ""
	}

	if err := w.actions.UpsertAction(ctx, model.Action{
		IdentityKey: identityKey,
		DocumentID:  doc.ID,
		ThreadKey:   threadKey,
		Kind:        model.KindAwaitingMyReply,
		// Generated per thread, NOT a fixed template: a constant made all 434
		// of these rows render as the same card. The text is still never
		// hashed — NormalizeSummary discards it for this kind — so it may
		// change on every tick (it reports the thread's age) without moving
		// identity_key.
		Summary:         action.AwaitingReplySummary(doc, displayName, w.now()),
		CounterpartName: displayName,
		CounterpartType: entityType,
		DetectedBy:      model.DetectedStructural,
		Confidence:      1.0, // deterministic rule, not a probabilistic guess
		ObservedAt:      w.now().UTC(),
	}); err != nil {
		slog.Warn("structural signal worker: upsert action failed", "doc_id", doc.ID, "error", err)
		return outcomeFailed
	}
	if err := w.actions.EnsureOpenStatus(ctx, identityKey); err != nil {
		slog.Warn("structural signal worker: ensure open status failed", "doc_id", doc.ID, "error", err)
		return outcomeStatusFailed
	}
	return outcomeWritten
}

// isOutbound reports whether doc's direction indicates the ACCOUNT OWNER
// sent it — sms "sent"/"draft", call "outgoing". gmail direction is not
// checked here: it has no direction key at all, so it is handled entirely
// by CounterpartIdentity (which returns ok=false when "from" is the user's
// own address). Folding both concerns into one function per source type
// would blur CounterpartIdentity's "who is the counterpart" contract.
func isOutbound(doc *model.Document) bool {
	switch doc.SourceType {
	case model.SourceSMS:
		d, _ := doc.Metadata["direction"].(string)
		return d == "sent" || d == "draft"
	case model.SourceCallLog, model.SourceCallTranscript:
		d, _ := doc.Metadata["direction"].(string)
		return d == "outgoing"
	default:
		return false
	}
}
