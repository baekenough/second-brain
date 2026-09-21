package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/baekenough/second-brain/internal/calendarauto"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
)

const calendarAutomationLockKey = int64(0x534243414c415554)
const calendarAutomationMaxAttempts = 8

type CalendarAutomationStore struct{ pg *Postgres }

func NewCalendarAutomationStore(pg *Postgres) *CalendarAutomationStore {
	return &CalendarAutomationStore{pg: pg}
}

var _ calendarauto.Store = (*CalendarAutomationStore)(nil)

func (s *CalendarAutomationStore) Initialize(ctx context.Context) (time.Time, error) {
	_, err := s.pg.pool.Exec(ctx, `INSERT INTO calendar_automation_state(singleton) VALUES (TRUE) ON CONFLICT DO NOTHING`)
	if err != nil {
		return time.Time{}, errors.New("initialize calendar automation cutoff")
	}
	var cutoff time.Time
	err = s.pg.pool.QueryRow(ctx, `SELECT activated_at FROM calendar_automation_state WHERE singleton`).Scan(&cutoff)
	if err != nil {
		return time.Time{}, errors.New("read calendar automation cutoff")
	}
	return cutoff, nil
}

// A session lock needs a dedicated connection. If unlocking fails, destroy that
// connection rather than returning a possibly locked session to the pool.
func (s *CalendarAutomationStore) AcquireLock(ctx context.Context) (func(), bool, error) {
	conn, err := s.pg.pool.Acquire(ctx)
	if err != nil {
		return nil, false, errors.New("acquire calendar automation connection")
	}
	closeConn := func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Hijack().Close(closeCtx)
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, calendarAutomationLockKey).Scan(&acquired); err != nil {
		closeConn()
		return nil, false, errors.New("acquire calendar automation lock")
	}
	if !acquired {
		conn.Release()
		return func() {}, false, nil
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var unlocked bool
			if err := conn.QueryRow(unlockCtx, `SELECT pg_advisory_unlock($1)`, calendarAutomationLockKey).Scan(&unlocked); err != nil || !unlocked {
				closeConn()
				return
			}
			conn.Release()
		})
	}, true, nil
}

const calendarAutomationEligibility = `d.status = 'active'
    AND d.source_type IN ('gmail', 'sms', 'call')
    AND d.created_at >= $1 AND d.occurred_at >= $1
    AND (d.source_type <> 'call' OR d.metadata->>'transcription' = 'done')`

func (s *CalendarAutomationStore) ListJobs(ctx context.Context, cutoff time.Time, limit int) ([]calendarauto.Job, error) {
	if cutoff.IsZero() || limit <= 0 {
		return nil, errors.New("invalid calendar automation query bounds")
	}
	if limit > 1000 {
		limit = 1000
	}
	_, err := s.pg.pool.Exec(ctx, `INSERT INTO calendar_automation_jobs(document_id)
    SELECT d.id FROM documents d WHERE `+calendarAutomationEligibility+`
    AND NOT EXISTS (SELECT 1 FROM calendar_automation_jobs j WHERE j.document_id = d.id)
    ORDER BY d.created_at, d.id LIMIT $2 ON CONFLICT DO NOTHING`, cutoff, limit)
	if err != nil {
		return nil, errors.New("enqueue calendar automation documents")
	}
	rows, err := s.pg.pool.Query(ctx, `SELECT d.id, d.source_type, d.source_id, d.title, d.content,
    d.metadata, d.status, d.occurred_at, d.collected_at, d.created_at, d.updated_at,
    j.decision, j.candidate_event, j.attempts
    FROM calendar_automation_jobs j JOIN documents d ON d.id = j.document_id
    WHERE `+calendarAutomationEligibility+` AND j.status IN ('pending', 'retry')
    AND j.next_attempt_at <= now() AND j.attempts < $3
    ORDER BY j.next_attempt_at, d.created_at, d.id LIMIT $2`, cutoff, limit, calendarAutomationMaxAttempts)
	if err != nil {
		return nil, errors.New("query calendar automation jobs")
	}
	defer rows.Close()
	var jobs []calendarauto.Job
	for rows.Next() {
		d := new(model.Document)
		j := calendarauto.Job{Document: d}
		var metadata, decision, event []byte
		if err := rows.Scan(&d.ID, &d.SourceType, &d.SourceID, &d.Title, &d.Content,
			&metadata, &d.Status, &d.OccurredAt, &d.CollectedAt, &d.CreatedAt, &d.UpdatedAt,
			&decision, &event, &j.Attempts); err != nil {
			return nil, errors.New("scan calendar automation job")
		}
		if len(metadata) > 0 && json.Unmarshal(metadata, &d.Metadata) != nil {
			return nil, errors.New("decode calendar automation metadata")
		}
		if len(decision) > 0 && json.Unmarshal(decision, &j.Decision) != nil {
			return nil, errors.New("decode calendar automation decision")
		}
		if len(event) > 0 && json.Unmarshal(event, &j.Event) != nil {
			return nil, errors.New("decode calendar automation event")
		}
		jobs = append(jobs, j)
	}
	if rows.Err() != nil {
		return nil, errors.New("read calendar automation jobs")
	}
	return jobs, nil
}

func (s *CalendarAutomationStore) SaveDecision(ctx context.Context, id uuid.UUID, d calendarauto.Decision) error {
	return s.saveJSON(ctx, id, `UPDATE calendar_automation_jobs SET decision = COALESCE(decision, $2::jsonb), updated_at = now() WHERE document_id = $1 AND status IN ('pending', 'retry')`, d)
}

func (s *CalendarAutomationStore) SaveEvent(ctx context.Context, id uuid.UUID, e calendarauto.Event) error {
	return s.saveJSON(ctx, id, `UPDATE calendar_automation_jobs SET candidate_event = COALESCE(candidate_event, $2::jsonb), updated_at = now() WHERE document_id = $1 AND status IN ('pending', 'retry')`, e)
}

func (s *CalendarAutomationStore) saveJSON(ctx context.Context, id uuid.UUID, query string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return errors.New("encode calendar automation result")
	}
	tag, err := s.pg.pool.Exec(ctx, query, id, data)
	if err != nil || tag.RowsAffected() != 1 {
		return errors.New("persist calendar automation result")
	}
	return nil
}

func (s *CalendarAutomationStore) Finish(ctx context.Context, id uuid.UUID, status, eventID string) error {
	switch status {
	case "created", "duplicate", "skipped", "review", "failed":
	default:
		return errors.New("invalid calendar automation completion status")
	}
	tag, err := s.pg.pool.Exec(ctx, `UPDATE calendar_automation_jobs
    SET status = $2, event_id = $3, retry_reason = '', updated_at = now()
    WHERE document_id = $1 AND status IN ('pending', 'retry')`, id, status, eventID)
	if err != nil || tag.RowsAffected() != 1 {
		return errors.New("finish calendar automation job")
	}
	return nil
}

// Only fixed stage codes are retained; upstream errors may contain personal data.
func (s *CalendarAutomationStore) Retry(ctx context.Context, id uuid.UUID, reason string) error {
	switch reason {
	case "decision_failed", "extraction_failed", "calendar_write_failed", "calendar_empty_event_id", "persistence":
	default:
		reason = "processing"
	}
	tag, err := s.pg.pool.Exec(ctx, `UPDATE calendar_automation_jobs
    SET attempts = attempts + 1,
        status = CASE WHEN attempts + 1 >= $3 THEN 'failed' ELSE 'retry' END,
        next_attempt_at = now() + LEAST(3600, 60 * power(2, LEAST(attempts, 6))) * interval '1 second',
        retry_reason = $2, updated_at = now()
    WHERE document_id = $1 AND status IN ('pending', 'retry')`, id, reason, calendarAutomationMaxAttempts)
	if err != nil || tag.RowsAffected() != 1 {
		return errors.New("retry calendar automation job")
	}
	return nil
}
