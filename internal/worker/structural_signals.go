package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ThreadLatestMessage is one row of StructuralSignalLister.ListLatestPerThread's
// result: the most recent document in a conversation thread. It used to also
// carry what StructuralSignalWorker needed to decide whether the thread was
// an "awaiting my reply" candidate (spec §7.1) and to describe it on an
// action card; that consumer is gone (see StructuralSignalWorker's doc
// comment), but the struct and the query below are read-only and still
// correct, so they are kept as-is rather than deleted along with their only
// caller.
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

// StructuralSignalLister provides the thread-grouped SQL query PgStructuralSignalLister
// implements. StructuralSignalWorker no longer calls it (see the worker's doc
// comment) — the interface remains so PgStructuralSignalLister keeps a typed
// contract and cmd/collector/main.go's wiring keeps compiling unchanged.
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

// listLatestPerThreadQuery groups gmail/sms/call documents into threads and
// returns only the single most recent document per thread (spec §7.1: "각
// 스레드에서 occurred_at 최대인 문서"). gmail threads are grouped by
// metadata->>'thread_id'; sms by metadata->>'contact_name'; call is grouped
// by metadata->>'contact_name' under the same "call:" thread_key prefix
// (spec §7.1 table). Pre-migration-033, this was call-log AND
// call-transcript grouped TOGETHER (both mapped to "call:" since they were
// two documents per call) — migration 033 unified them into model.SourceCall
// (one document per call), so the ELSE branch below now also covers 'call'
// without needing a dedicated WHEN; 'call-log'/'call-transcript' remain in
// the WHERE IN list defensively for the brief pre-migration window. sms
// documents flagged is_auth_like are excluded at the source (spec §7.4).
// No LLM call — pure SQL.
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
		  AND source_type IN ('gmail', 'sms', 'call', 'call-log', 'call-transcript')
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
	// *PgStructuralSignalLister. Kept for wiring compatibility — Tick no
	// longer calls it (see StructuralSignalWorker's doc comment) — and is
	// still validated non-nil so a caller does not silently depend on a
	// zero-value StructuralSignalLister that would panic differently later.
	Store StructuralSignalLister
	// Actions is the write side of the actions table (reused from Task 11).
	Actions ActionWriter
	// UserAddresses is config.UserEmailAddresses. Unused now that Tick no
	// longer infers gmail direction, but kept so cmd/collector/main.go's
	// existing wiring does not need to change.
	UserAddresses []string
	// Interval controls how often the worker polls. Defaults to 10 minutes.
	Interval time.Duration
	// BatchSize is the number of threads inspected per tick. Defaults to 500.
	BatchSize int
	// Now supplies the current instant. Unused now that Tick writes nothing,
	// kept for wiring compatibility.
	Now func() time.Time
}

// StructuralSignalWorker used to compute the "awaiting my reply" candidate
// action (spec §7.1) — the only structural signal this plan implemented.
//
// 2026-09-21 결정: 메일·SMS에 "답장하지 않았다"로 자동 생성되는 awaiting_my_reply
// 항목은 할일이 아니라 잡음이라고 판단해 더 이상 만들지 않기로 했다(migrations/038
// 이 기존에 열려 있던 행도 무시 처리한다). 이 워커가 만들던 신호는 그것 하나뿐이었
// 으므로, 생성 경로(스레드 조회 → 상대방/스레드 식별 → identity_key 계산 →
// UpsertAction/EnsureOpenStatus 호출)를 통째로 들어냈다 — Tick은 이제 아무 SQL도
// 실행하지 않고 아무 액션도 쓰지 않는다.
//
// 타입·생성자·Run 시그니처는 cmd/collector/main.go의 배선을 건드리지 않기 위해
// 그대로 남겨 두었다. 배선 자체(고루틴 기동 여부)를 정리하는 것은 이번 변경의
// 범위가 아니다.
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

// Run blocks until ctx is cancelled. Tick is now a no-op (see the type's doc
// comment), so this loop no longer does any work — it is kept only so
// cmd/collector/main.go's existing goroutine wiring stays valid.
func (w *StructuralSignalWorker) Run(ctx context.Context) {
	slog.Info("structural signal worker started (awaiting_my_reply generation retired, tick is a no-op)")
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

// tickStats is what one Tick pass produced. Every field is always zero now
// that the worker generates nothing — the type is kept (rather than
// collapsing Tick to return nothing) so its signature does not change and a
// test can still assert "nothing was written" positively.
type tickStats struct {
	Listed  int
	Written int
}

// Tick is exported (unlike ExtractionWorker's unexported tick) so tests can
// drive one pass deterministically without a ticker.
//
// It performs no read and no write: the only signal this worker ever
// produced was awaiting_my_reply, and that generation path — thread listing,
// counterpart/thread-key resolution, identity_key construction, and the
// UpsertAction/EnsureOpenStatus calls — has been removed entirely (see the
// type's doc comment). Calling w.store.ListLatestPerThread here would only
// spend a database round trip on behalf of a candidate this worker will
// never write, so it is skipped rather than kept as dead instrumentation.
func (w *StructuralSignalWorker) Tick(_ context.Context) tickStats {
	return tickStats{}
}
