package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/baekenough/second-brain/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pgvector/pgvector-go"
)

// summaryCoverageCache is a process-level cache for SummaryCoverageRatio results.
// It avoids a per-query COUNT(*) on large tables while keeping the gate responsive
// to backfill progress. TTL is 60 s by default; see summaryCoverageTTL.
const summaryCoverageTTL = 60 * time.Second

// DocumentStore provides document persistence and search operations.
type DocumentStore struct {
	pg *Postgres

	// coverage cache fields — protects SummaryCoverageRatio from per-query scans.
	coverageMu        sync.Mutex
	coverageRatio     float64
	coverageFetchedAt time.Time
}

// NewDocumentStore returns a DocumentStore backed by the given Postgres instance.
func NewDocumentStore(pg *Postgres) *DocumentStore {
	return &DocumentStore{pg: pg}
}

// callDupCheckQuery is the existence check used by Upsert/UpsertTracked/
// AttachTranscript to detect call documents that share identical transcript
// content under a different source_id (issue #134, generalized to
// source_type='call' by migration 033's call-log/call-transcript unification —
// see model.SourceCall's doc comment). The query is a SELECT 1 probe
// (LIMIT 1) so it avoids a full scan. 'call-transcript' is matched alongside
// 'call' defensively for the brief window before migration 033 has run against
// a given database; every ROW WhisperCollector emits post-unification carries
// source_type='call'.
//
// Parameters:
//
//	$1 — content (text)
//	$2 — source_id to exclude (the current document's own source_id)
//
// The query intentionally targets only status='active' rows so that a previously
// soft-deleted duplicate does not block re-insertion of a renamed recording.
const callDupCheckQuery = `
	SELECT 1
	FROM documents
	WHERE source_type IN ('call', 'call-transcript')
	  AND status       = 'active'
	  AND content      = $1
	  AND source_id   <> $2
	LIMIT 1`

// classificationProtectedMetadataKeys lists every documents.metadata key the
// classification pipeline owns — internal/classify.Result.Metadata
// (segment/retention/classifier/classifier_p/classified_at/
// classifier_gate_checked_at/needs_review/gate),
// IncrementClassificationAttempts (classifier_attempts), and golden.go's
// UpsertJudgments (retention/classifier/classified_at, a subset already
// covered above) — see upsertMetadataMergeSQL's doc comment for why these
// specifically must survive a collector re-upsert. Anyone adding a new
// classification-owned metadata key MUST add it here too, or a future
// re-collection will silently erase it (the exact bug this constant fixes).
const classificationProtectedMetadataKeys = `'segment', 'retention', 'classifier', 'classifier_p', 'classified_at', 'classifier_gate_checked_at', 'needs_review', 'gate', 'classifier_attempts'`

// upsertMetadataMergeSQL is the `metadata` assignment shared by Upsert and
// UpsertTracked's ON CONFLICT DO UPDATE SET clause.
//
// Collector re-upserts (Upsert/UpsertTracked) treat metadata as a FULL
// REPLACEMENT snapshot for every key the collector itself owns (sender,
// content, direction, label_ids, ...) — a re-synced SMS/Gmail/call document's
// metadata should reflect exactly what the collector observed this time, not
// an accumulation of every value ever seen. A plain `metadata =
// EXCLUDED.metadata` therefore used to also silently discard whatever this
// package's OWN classification pipeline had written onto that same row
// (retention/classifier/...) between collections, since EXCLUDED.metadata
// only ever contains collector-owned keys — the classifier's tags are simply
// absent from it, not intentionally cleared. That is the root cause of a
// golden-set "relevant"→retention=keep judgment reverting to disposable
// after the document's next re-collection: the classifier="user" key that
// protected it (see mergeClassificationMetadataQuery's WHERE guard) was
// wiped before the guard ever got a chance to see it.
//
// The fix: overlay ONLY classificationProtectedMetadataKeys from the
// EXISTING (pre-conflict) row onto EXCLUDED.metadata, so those specific keys
// survive untouched while every other key (collector-owned) is replaced
// wholesale by the incoming value, exactly as before this fix. `documents.
// metadata` here reads the PRE-update row, the same established idiom
// AttachTranscript already relies on for its own metadata merge (ON
// CONFLICT DO UPDATE SET expressions evaluate against the OLD row, not the
// RETURNING clause's POST-update value — see UpsertTracked's doc comment on
// why RETURNING itself cannot use this trick).
//
// This is deliberately NOT the same shape as AttachTranscript's `documents.
// metadata || EXCLUDED.metadata` (see its doc comment): AttachTranscript's
// incoming metadata is always a deliberate PATCH of a few transcript-only
// keys, so merging the whole existing object underneath it is correct and
// leaves nothing stale. Upsert/UpsertTracked's incoming metadata is the
// collector's full snapshot, so doing the same full merge there would leave
// every collector-owned key the collector no longer reports (e.g. a removed
// label, a corrected sender) stuck at its stale value forever — only the
// finite classification-owned key set above needs protecting, not the whole
// object.
//
// 기존 metadata 는 existingMetadataObjectSQL 로 정규화해서 읽는다(#292 리뷰).
// SQL NULL·jsonb null·배열·스칼라에 jsonb_each 를 부르면 "cannot call
// jsonb_each on a non-object" 로 upsert 전체가 실패한다.
const upsertMetadataMergeSQL = `EXCLUDED.metadata || COALESCE((
			SELECT jsonb_object_agg(kv.key, kv.value)
			FROM jsonb_each(` + existingMetadataObjectSQL + `) AS kv(key, value)
			WHERE kv.key = ANY (ARRAY[` + classificationProtectedMetadataKeys + `])
		), '{}'::jsonb)`

// existingMetadataObjectSQL 은 기존 행의 metadata 를 객체로 정규화한다. 객체가
// 아니면(SQL NULL·jsonb null·배열·스칼라) 빈 객체로 본다. 열 기본값은 '{}'
// 이지만 NOT NULL 제약이 없고, Go 쪽 nil map 은 json.Marshal 로 jsonb null 이
// 되므로 실제로 생길 수 있는 값이다. COALESCE(NULLIF(…, 'null'), '{}') 보다
// 넓게 잡은 것은 배열·스칼라도 jsonb_each·|| 에서 같은 문제를 내기 때문이다.
const existingMetadataObjectSQL = `CASE WHEN jsonb_typeof(documents.metadata) = 'object' THEN documents.metadata ELSE '{}'::jsonb END`

// 통화 전사 보호(#292) — Upsert·UpsertTracked 의 ON CONFLICT 가 함께 쓴다.
//
// 통화 로그(smsmap.MapCall, transcription="none")·녹음 업로드(ingest/recording,
// "pending")·전사(WhisperCollector → AttachTranscript, "done")는 source_id
// 하나(call-log:{dateMs}:{numHash}:{durHash})를 같이 쓴다(마이그레이션 033,
// model.SourceCall 주석). 전사가 붙은 뒤 같은 통화 로그나 녹음이 다시 오면
// (앱 재설치·커서 초기화로 전량 재전송, XML SMS 백업 재수집, 녹음 재업로드)
// 예전 ON CONFLICT 는 content 를 4줄 요약으로, metadata 를 요약의 스냅숏으로
// 통째로 바꿨다. 전사 원장(transcription_ledger)이 같은 오디오의 재전사를
// 막으므로 그 전사는 영구히 사라졌다.
//
// 보호 조건(callTranscriptKeptSQL) — 넷 다 참이어야 한다:
//   - 기존 행이 source_type='call' 이다. 다른 소스는 전혀 건드리지 않는다.
//   - 기존 metadata 가 객체다. SQL NULL·jsonb null·배열이면 전사 상태를 알 수
//     없고(마이그레이션 033 이 모든 통화 문서에 transcription 키를 채웠으므로
//     정상 행에서는 생기지 않는다), 예전처럼 보호했다가는 metadata 가 NULL 로
//     남거나 null||{} 가 배열이 된다. 보통 upsert 로 처리해 객체로 되돌린다.
//   - 기존 행의 transcription 이 none·pending 이 아니다. 값의 실제 집합은
//     none·pending·done 셋이고(model.SourceCall 주석, smsmap.MapCall,
//     ingest_recording.go, whisper.go), 키가 없는 행은 033 이전의 레거시
//     전사 문서뿐이다 — classify.evaluateCall 도 키 없음을 done 과 같이 본다.
//     그래서 "none·pending 이 아니면 전사가 있다" 로 판정한다. 모르는 새 값이
//     생겨도 보호 쪽으로 기운다(덮어서 잃는 것보다 안전하다).
//   - 들어오는 행의 transcription 이 none 또는 pending 이다. 즉 통화 로그나
//     녹음 업로드가 보낸 요약이다. WhisperCollector 의 단독 전사 문서
//     (transcript:{relPath}, 키 없음)끼리의 upsert 는 전사가 전사를 고치는
//     것이므로 보호하지 않는다.
//
// 보호할 때:
//   - content·embedding·embedding_version 은 기존 값을 둔다. 들어온 요약의
//     임베딩이 전사의 임베딩을 덮으면 벡터 검색이 전사를 못 찾는다.
//   - metadata 는 기존 행 전체를 두고, 들어온 것이 통화 로그 재수집
//     (transcription="none")일 때만 통화 로그가 권위를 갖는 키
//     (callLogRefreshableMetadataKeys)를 들어온 값으로 덧씌운다. 녹음 재업로드
//     ("pending")는 아무것도 덧씌우지 않는다 — 녹음 핸들러는 direction 을
//     incoming 으로 박고(앱이 방향을 보내지 않는다) contact_name 이 비어 올 수
//     있어서, 덧씌우면 전사된 발신 통화가 수신으로 바뀌고 연락처가 사라진다(#292
//     리뷰). 녹음 쪽은 통화 로그 키의 권위가 아니다. 기존 행 전체를
//     두는 이유: 전사 쪽 키는 whisper.go 버전·사이드카·033 병합(옛 전사 문서의
//     metadata 를 통째로 합침)에 따라 달라서 목록으로 다 적을 수 없다. 목록에
//     없는 키 하나를 잃는 것보다 통화 로그 키 몇 개만 갱신하는 편이 안전하다.
//     분류 키(classificationProtectedMetadataKeys)도 기존 행에 있으므로 그대로
//     남는다.
//   - title 은 metadata 와 같은 원칙이다: 통화 로그 재수집("none")이면 들어온
//     제목(통화 로그가 권위, AttachTranscript 도 통화 로그 쪽 제목을 남긴다),
//     녹음 재업로드("pending")면 기존 제목을 둔다(녹음 쪽 제목은 늘 "incoming
//     통화 …" 이다).
//   - occurred_at·collected_at 은 예전과 같다. occurred_at 은 통화 로그의
//     dateMs 가 권위이고, 같은 source_id 면 dateMs 도 같다.
//   - content_changed 는 false 가 된다(upsertTrackedRow 의 RETURNING 이 갱신
//     후 content 와 비교한다). 그래서 청크 교체·재임베딩이 일어나지 않는다.
const callTranscriptKeptSQL = `(documents.source_type = 'call'
			AND jsonb_typeof(documents.metadata) = 'object'
			AND COALESCE(documents.metadata->>'transcription', '') NOT IN ('none', 'pending')
			AND COALESCE(EXCLUDED.metadata->>'transcription', '') IN ('none', 'pending'))`

// callLogRefreshableMetadataKeys 는 전사가 붙은 통화 문서에서도 통화 로그 재수집이
// 갱신해도 되는 metadata 키다. 모두 smsmap.MapCall 이 통화 로그에서 만드는
// 키이고, 마이그레이션 033 도 병합 뒤 이 넷을 통화 로그 쪽 값으로 다시 박았다
// ("통화 로그가 이 키들의 권위"). 연락처를 나중에 저장하면 contact_name 이
// 바뀌고, PII 플래그를 켜면 contact_name·number 가 가림 토큰으로 바뀐다 —
// 둘 다 반영돼야 한다. duration_seconds 는 source_id(durHash)에 들어 있어
// 같은 문서라면 값이 같다.
//
// 넣지 않은 키와 이유:
//   - transcription·audio_file·recording_type: 전사·녹음 상태다. 통화 로그의
//     "none" 이나 재업로드의 새 파일 이름으로 바꾸면 전사가 없는 문서처럼
//     보이거나 전사한 오디오와 연결이 끊긴다.
//   - pii_name_redacted: content 가 가려졌는지를 나타낸다. 보호 중에는
//     content(전사)를 바꾸지 않으므로 통화 로그 쪽 값을 옮기면 사실과 달라진다.
const callLogRefreshableMetadataKeys = `'contact_name', 'direction', 'duration_seconds', 'number'`

// callFromCallLogSQL 은 들어온 행이 통화 로그 재수집(transcription="none")인지
// 본다. 보호 조건 안에서만 쓰므로 들어온 값은 none 아니면 pending 이다.
const callFromCallLogSQL = `EXCLUDED.metadata->>'transcription' = 'none'`

// callTranscriptUpsertSetSQL 은 Upsert·UpsertTracked 의 ON CONFLICT DO UPDATE
// SET 가운데 보호 대상 열(title·content·metadata·embedding·embedding_version)
// 이다. 보호 조건이 거짓이면 예전 식과 똑같다(metadata 만 기존 행 정규화가
// 더해졌다 — existingMetadataObjectSQL).
const callTranscriptUpsertSetSQL = `
			title        = CASE WHEN ` + callTranscriptKeptSQL + ` AND NOT (` + callFromCallLogSQL + `)
			               THEN documents.title ELSE EXCLUDED.title END,
			content      = CASE WHEN ` + callTranscriptKeptSQL + `
			               THEN documents.content ELSE EXCLUDED.content END,
			metadata     = CASE WHEN ` + callTranscriptKeptSQL + `
			               THEN documents.metadata || CASE WHEN ` + callFromCallLogSQL + ` THEN COALESCE((
			                   SELECT jsonb_object_agg(kv.key, kv.value)
			                   FROM jsonb_each(EXCLUDED.metadata) AS kv(key, value)
			                   WHERE kv.key = ANY (ARRAY[` + callLogRefreshableMetadataKeys + `])
			               ), '{}'::jsonb) ELSE '{}'::jsonb END
			               ELSE ` + upsertMetadataMergeSQL + ` END,
			embedding    = CASE WHEN ` + callTranscriptKeptSQL + `
			               THEN documents.embedding
			               ELSE COALESCE(EXCLUDED.embedding, documents.embedding) END,
			embedding_version = CASE WHEN ` + callTranscriptKeptSQL + ` THEN documents.embedding_version
			                    WHEN EXCLUDED.embedding IS NOT NULL THEN EXCLUDED.embedding_version
			                    ELSE documents.embedding_version END,`

// UpsertTracked is identical to Upsert but additionally returns a bool that
// indicates whether the document's content actually changed. Callers that
// perform post-upsert work (chunking, embedding) can use this to skip
// expensive operations when a document arrives unchanged.
//
// Change detection uses a WITH CTE that captures the pre-update content in the
// same MVCC snapshot as the INSERT … ON CONFLICT statement, then compares it
// against the incoming value in RETURNING. This avoids two PostgreSQL pitfalls:
//  1. EXCLUDED cannot be referenced in the RETURNING clause (only valid inside
//     ON CONFLICT DO UPDATE SET/WHERE).
//  2. `documents.col` in RETURNING reflects the post-update value, so a naive
//     `documents.content IS DISTINCT FROM EXCLUDED.content` would always be
//     false after the UPDATE overwrites the column.
//
// metadata on conflict is EXCLUDED.metadata with classificationProtectedMetadataKeys
// overlaid back from the existing row — see upsertMetadataMergeSQL's doc
// comment for why a bare `metadata = EXCLUDED.metadata` used to silently
// erase a golden-set "user" retention judgment on the document's next
// re-collection.
//
// 통화 전사 보호(#292): 전사가 붙은 통화 문서에 같은 source_id 의 통화 로그·
// 녹음 요약이 다시 오면 content·전사 metadata·임베딩을 남기고
// contentChanged=false 를 돌려준다 — callTranscriptKeptSQL 주석 참고.
//
// *DocumentStore satisfies the api.IngestMessagesUpserter interface via this method.
func (s *DocumentStore) UpsertTracked(ctx context.Context, doc *model.Document) (contentChanged bool, err error) {
	if err := s.preUpsertTrackedChecks(ctx, doc); err != nil {
		return false, err
	}
	return upsertTrackedRow(ctx, s.pg.pool, doc)
}

// UpsertTrackedWithChunks 는 UpsertTracked 와 같은 upsert 를 하되, 내용이
// 바뀌었으면(contentChanged) 그 문서의 청크 교체까지 **한 트랜잭션**에서 한다
// (#290). buildChunks 는 upsert 가 doc.ID 를 채운 뒤, 내용이 바뀐 경우에만
// 호출된다. 돌려받은 청크의 DocumentID 는 무시하고 doc.ID 를 쓴다.
//
// 왜 한 트랜잭션인가: upsert(autocommit)와 청크 교체(별도 트랜잭션)를 따로
// 하면, 청크 교체만 실패했을 때 문서 행은 새 내용으로 이미 커밋돼 있다. 호출자가
// 일시 오류로 보고 같은 레코드를 다시 보내면 upsert 는 "내용이 같다"
// (contentChanged=false)를 돌려주고 청크 교체를 건너뛴다 — 그 문서는 청크 없이
// (또는 옛 내용의 청크로) 영구히 남는다. 청크 백필은 embedding IS NULL 인 청크만
// 채우고, 청크가 아예 없는 문서는 되살리지 않는다. 한 트랜잭션으로 묶으면 "문서
// 행이 새 내용이다" 이면 "청크도 새 내용이다" 가 항상 성립하므로, 실패 뒤
// 재전송은 언제나 처음부터 다시 한다.
//
// 내용이 바뀌었는데 buildChunks 가 빈 슬라이스를 돌려주면(빈 본문) 옛 청크를
// 지우기만 한다 — 옛 내용의 청크가 새 내용의 문서에 매달려 있으면 안 된다.
//
// 동시성: upsert 가 문서 행에 FOR NO KEY UPDATE 잠금을 잡은 뒤에 청크를
// 교체하고, ChunkStore.ReplaceDocument 는 청크를 지우기 전에 같은 행을
// FOR UPDATE 로 잠근다. 그래서 같은 문서의 청크 교체 둘이 겹치지 않는다
// (겹치면 늦은 쪽이 chunk_index 유일 제약 23505 로 실패했다). 잠금 순서는
// 양쪽 모두 "문서 행 → 청크 행" 이다(ReplaceDocument 주석의 교착 분석).
//
// 사전 점검(checkDuplicateArrival, 통화 중복 전사 확인)은 읽기 전용이라
// 트랜잭션 밖에서 풀로 한다. ErrDuplicateTranscript 는 UpsertTracked 처럼
// 감싸지 않고 돌려준다. 그 밖의 오류는 %w 로 감싸므로 *pgconn.PgError·context
// 오류를 errors.As/Is 로 꺼낼 수 있다.
func (s *DocumentStore) UpsertTrackedWithChunks(
	ctx context.Context,
	doc *model.Document,
	buildChunks func(*model.Document) []Chunk,
) (contentChanged bool, err error) {
	if err := s.preUpsertTrackedChecks(ctx, doc); err != nil {
		return false, err
	}

	tx, err := s.pg.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("upsert with chunks: begin tx: %w", err)
	}
	defer func() {
		// 커밋한 뒤에는 아무 일도 하지 않는다. 오류 경로에서는 문서 행도 되돌린다.
		_ = tx.Rollback(ctx)
	}()

	contentChanged, err = upsertTrackedRow(ctx, tx, doc)
	if err != nil {
		return false, fmt.Errorf("upsert with chunks: upsert: %w", err)
	}
	if contentChanged && buildChunks != nil {
		if err := replaceChunksTx(ctx, tx, doc.ID, buildChunks(doc)); err != nil {
			return false, fmt.Errorf("upsert with chunks: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("upsert with chunks: commit: %w", err)
	}
	return contentChanged, nil
}

// preUpsertTrackedChecks 는 UpsertTracked·UpsertTrackedWithChunks 가 쓰기 전에
// 하는 점검이다. 경고 두 개는 쓰기를 막지 않고, 통화 중복 전사면
// ErrDuplicateTranscript 를 돌려준다.
func (s *DocumentStore) preUpsertTrackedChecks(ctx context.Context, doc *model.Document) error {
	// Recurrence guards (migration 027 background): warn on a container or
	// deprecated source_type, and warn on a possible cross-source duplicate
	// arrival. Both are non-blocking — see document_source_guard.go package
	// doc for why neither guard fails the write.
	checkSourceTypeGuard(doc)
	s.checkDuplicateArrival(ctx, doc)

	// Duplicate guard: call content dedup (issue #134, generalized to
	// source_type='call' by migration 033 — see callDupCheckQuery's doc
	// comment). In practice this only ever fires for transcript content: a
	// plain call-log summary embeds its own timestamp-to-the-second, so two
	// distinct real calls essentially cannot produce byte-identical content.
	if doc.SourceType == model.SourceCall {
		var exists int
		qErr := s.pg.pool.QueryRow(ctx, callDupCheckQuery,
			doc.Content,
			doc.SourceID,
		).Scan(&exists)
		switch {
		case qErr == nil:
			slog.Info("store: skipping duplicate call content",
				"source_id", doc.SourceID,
				"content_len", len(doc.Content),
			)
			return ErrDuplicateTranscript
		case isNoRows(qErr):
			// No duplicate — proceed with the normal upsert.
		default:
			return fmt.Errorf("call dup check: %w", qErr)
		}
	}
	return nil
}

// rowQuerier 는 *pgxpool.Pool 과 pgx.Tx 가 함께 만족하는 QueryRow 다.
// upsertTrackedRow 가 autocommit(풀)과 트랜잭션(UpsertTrackedWithChunks)에서
// 같은 문장 코드를 쓰게 하려는 것이다.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// upsertTrackedRow 는 UpsertTracked 의 INSERT … ON CONFLICT 문장을 db 위에서
// 실행하고 contentChanged 를 돌려준다. 변경 감지 방식은 UpsertTracked 문서
// 주석과 아래 주석을 본다.
func upsertTrackedRow(ctx context.Context, db rowQuerier, doc *model.Document) (contentChanged bool, err error) {
	meta, err := json.Marshal(doc.Metadata)
	if err != nil {
		return false, fmt.Errorf("marshal metadata: %w", err)
	}

	var embeddingArg interface{}
	if len(doc.Embedding) > 0 {
		embeddingArg = pgvector.NewVector(doc.Embedding)
	}

	// embedding_version: 빈 문자열이면 NULL 을 보내고, SQL 쪽에서 기존 값을
	// 유지한다(마이그레이션 037 참고). 임베딩을 만들지 않은 재수집 upsert 가
	// 이미 기록된 버전을 지우면 안 되기 때문이다.
	var embeddingVersionArg interface{}
	if doc.EmbeddingVersion != "" {
		embeddingVersionArg = doc.EmbeddingVersion
	}

	// Change detection via CTE pre-update snapshot.
	//
	// Why CTE and not RETURNING + EXCLUDED:
	//   PostgreSQL does not allow EXCLUDED references in the RETURNING clause
	//   (only valid inside ON CONFLICT DO UPDATE SET/WHERE). Additionally,
	//   RETURNING evaluates after the UPDATE is applied, so `documents.content`
	//   in RETURNING would already hold the new (post-update) value — making an
	//   IS DISTINCT FROM comparison there always false.
	//
	// Solution: the `prev` CTE runs a SELECT in the same statement snapshot
	// (PostgreSQL CTE semantics: all CTEs and the main DML share one MVCC
	// snapshot), capturing the pre-update content before the INSERT/UPDATE
	// fires. On INSERT the `prev` row is empty → COALESCE(old_content, '') →
	// compared against the stored content(저장된 값), which will differ → content_changed=true.
	// On UPDATE with identical content: old=new → content_changed=false.
	// On UPDATE with changed content: old≠new → content_changed=true.
	// xmax::text::bigint=0 signals a fresh INSERT (belt-and-suspenders: a new
	// row cannot have unchanged content anyway).
	//
	// 비교 대상은 들어온 값($4)이 아니라 RETURNING 의 documents.content, 곧
	// **갱신 후 실제로 저장된 값**이다(#292). 위 2번 함정(RETURNING 은 갱신 후
	// 값을 본다)을 여기서는 일부러 이용한다. 통화 전사 보호
	// (callTranscriptKeptSQL)가 기존 전사를 남기면 저장된 content 는 이전과 같고,
	// 그래서 content_changed=false 가 되어 호출자가 청크를 요약으로 바꾸거나
	// 재임베딩하지 않는다. $4 와 비교하면 들어온 요약과 옛 전사가 다르므로
	// true 가 되어, 문서 행은 지켜 놓고 청크만 요약으로 바꾸는 반쪽 덮어쓰기가
	// 된다. 보호가 없는 경우에는 저장된 값이 곧 $4 라 예전과 결과가 같다.
	const q = `
		WITH prev AS (
			SELECT content AS old_content
			FROM documents
			WHERE source_type = $1 AND source_id = $2
		)
		INSERT INTO documents
			(source_type, source_id, title, content, metadata, embedding, occurred_at, collected_at, embedding_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (source_type, source_id) DO UPDATE SET
			` + callTranscriptUpsertSetSQL + `
			occurred_at  = COALESCE(EXCLUDED.occurred_at, documents.occurred_at),
			collected_at = EXCLUDED.collected_at,
			status       = 'active',
			deleted_at   = NULL,
			updated_at   = now()
		RETURNING id, created_at, updated_at,
		          (xmax::text::bigint = 0) AS was_insert,
		          COALESCE((SELECT old_content FROM prev), '') IS DISTINCT FROM documents.content AS content_changed`

	var wasInsert bool
	row := db.QueryRow(ctx, q,
		doc.SourceType,
		doc.SourceID,
		doc.Title,
		doc.Content,
		meta,
		embeddingArg,
		doc.OccurredAt,
		doc.CollectedAt,
		embeddingVersionArg,
	)
	if err := row.Scan(&doc.ID, &doc.CreatedAt, &doc.UpdatedAt, &wasInsert, &contentChanged); err != nil {
		return false, err
	}
	// Fresh INSERT: content is always new. The CTE comparison also yields true
	// in this case (prev is empty → COALESCE '' IS DISTINCT FROM $4), but we
	// guard explicitly so any empty-string edge case cannot produce a false skip.
	if wasInsert {
		contentChanged = true
	}
	return contentChanged, nil
}

// Upsert inserts a document or updates it when (source_type, source_id) already exists.
// On conflict the status is reset to 'active' (handles re-appearance of previously deleted files).
//
// Duplicate call content guard: for source_type='call' only, a cheap
// pre-insert existence check is performed. When an active document with
// identical content but a DIFFERENT source_id already exists, the upsert is
// skipped and ErrDuplicateTranscript is returned (the caller may safely ignore
// or log it). A same-source_id re-upsert is NOT affected: the ON CONFLICT path
// handles it normally even when the content is identical.
//
// metadata on conflict is EXCLUDED.metadata with classificationProtectedMetadataKeys
// overlaid back from the existing row (see upsertMetadataMergeSQL's doc
// comment) — every other metadata key is replaced wholesale by the incoming
// collector snapshot, same as before this protection was added.
//
// 통화 전사 보호(#292)도 UpsertTracked 와 똑같이 적용된다(녹음 재업로드가
// 이 경로다) — callTranscriptKeptSQL 주석 참고.
func (s *DocumentStore) Upsert(ctx context.Context, doc *model.Document) error {
	// Recurrence guards (migration 027 background): warn on a container or
	// deprecated source_type, and warn on a possible cross-source duplicate
	// arrival. Both are non-blocking — see document_source_guard.go package
	// doc for why neither guard fails the write.
	checkSourceTypeGuard(doc)
	s.checkDuplicateArrival(ctx, doc)

	// Duplicate guard: call content dedup (issue #134, generalized to
	// source_type='call' by migration 033 — see callDupCheckQuery's doc
	// comment). Only applied when source_type is 'call'. Other source types
	// are unaffected. Same-source_id re-upserts bypass this check because the
	// query excludes the document's own source_id; the ON CONFLICT path below
	// handles them.
	if doc.SourceType == model.SourceCall {
		var exists int
		err := s.pg.pool.QueryRow(ctx, callDupCheckQuery,
			doc.Content,
			doc.SourceID,
		).Scan(&exists)
		switch {
		case err == nil:
			// A duplicate row was found — skip the insert.
			slog.Info("store: skipping duplicate call content",
				"source_id", doc.SourceID,
				"content_len", len(doc.Content),
			)
			return ErrDuplicateTranscript
		case isNoRows(err):
			// No duplicate — proceed with the normal upsert.
		default:
			return fmt.Errorf("call dup check: %w", err)
		}
	}

	meta, err := json.Marshal(doc.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	var embeddingArg interface{}
	if len(doc.Embedding) > 0 {
		embeddingArg = pgvector.NewVector(doc.Embedding)
	}

	// embedding_version: 빈 문자열이면 NULL 을 보내고, SQL 쪽에서 기존 값을
	// 유지한다(마이그레이션 037 참고). 임베딩을 만들지 않은 재수집 upsert 가
	// 이미 기록된 버전을 지우면 안 되기 때문이다.
	var embeddingVersionArg interface{}
	if doc.EmbeddingVersion != "" {
		embeddingVersionArg = doc.EmbeddingVersion
	}

	// occurred_at is the original event time (email date, calendar start, etc.).
	// NULL is stored when the collector has no event-time concept; COALESCE
	// in ORDER BY clauses falls back to collected_at for those rows.
	// On conflict we update occurred_at only when the incoming value is non-NULL
	// so that a re-collection without a timestamp does not erase a previously
	// parsed value.
	const q = `
		INSERT INTO documents
			(source_type, source_id, title, content, metadata, embedding, occurred_at, collected_at, embedding_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (source_type, source_id) DO UPDATE SET
			` + callTranscriptUpsertSetSQL + `
			occurred_at  = COALESCE(EXCLUDED.occurred_at, documents.occurred_at),
			collected_at = EXCLUDED.collected_at,
			status       = 'active',
			deleted_at   = NULL,
			updated_at   = now()
		RETURNING id, created_at, updated_at`

	row := s.pg.pool.QueryRow(ctx, q,
		doc.SourceType,
		doc.SourceID,
		doc.Title,
		doc.Content,
		meta,
		embeddingArg,
		doc.OccurredAt,
		doc.CollectedAt,
		embeddingVersionArg,
	)
	return row.Scan(&doc.ID, &doc.CreatedAt, &doc.UpdatedAt)
}

// ErrDuplicateTranscript is returned by Upsert when a call-transcript document
// with identical content already exists under a different source_id.
// Callers in the collector pipeline should log and skip this document; they
// MUST NOT treat it as a fatal error that aborts the entire collection cycle.
var ErrDuplicateTranscript = fmt.Errorf("store: call-transcript with identical content already exists (different source_id)")

// isNoRows reports whether err is a pgx "no rows" sentinel. Extracted to a
// helper so the Upsert guard remains readable without importing pgx directly.
func isNoRows(err error) bool {
	return err == pgx.ErrNoRows
}

// AttachTranscript merges a whisper transcript into the call document at
// (doc.SourceType, doc.SourceID) — normally the call-log-formula SourceID
// computed by internal/collector/whisper.go's callLogMergeSourceID, so the
// transcript lands on the SAME document ingest_recording.go created rather
// than a second, unlinked one (model.SourceCall's doc comment: "통화 1건 =
// 문서 1건").
//
// Unlike Upsert/UpsertTracked, on conflict this method:
//   - REPLACES content and embedding — the transcript supersedes the short
//     call-log summary as the document's substantive content.
//   - MERGES metadata via `documents.metadata || EXCLUDED.metadata` rather
//     than replacing it wholesale: doc.Metadata is treated as a PATCH
//     (transcription, transcript_source_id, model, language, diarization,
//     speaker_count, ...), so call-log-only keys the transcript never knows
//     about (contact_name/direction/duration_seconds from smsmap.MapCall,
//     any later retention tag) survive the merge. On key collision the
//     patch's value wins (jsonb `||` is right-biased). This full merge is
//     safe here specifically because doc.Metadata is always a small,
//     deliberate patch — it is NOT the same shape as Upsert/UpsertTracked's
//     merge (upsertMetadataMergeSQL), whose incoming metadata is the
//     collector's FULL replacement snapshot; a full `||` merge there would
//     leave every collector-owned key the collector no longer reports (a
//     removed label, a corrected sender) stuck at its stale value forever,
//     so those two protect only the classification-owned key set instead of
//     merging the whole object.
//   - Does NOT touch title on conflict — the call-log title ("incoming 통화
//     상대") stays more useful than the transcript's raw filename-stem
//     title. collected_at IS still refreshed to EXCLUDED, matching Upsert's
//     "last time this document was touched" semantics.
//   - COALESCEs occurred_at onto the EXISTING value first, not the incoming
//     one — the call-log's dateMs-derived timestamp is authoritative;
//     WhisperCollector's filename-parsed occurredAt only matters for the
//     INSERT branch below (no existing call document to merge into).
//
// When no document exists yet at (source_type, source_id) — e.g. a recording
// was transcribed before its call-log document was ever created — this
// INSERTs a new one using doc's own Title/OccurredAt/Metadata, identical in
// shape to what Upsert would have produced for a standalone transcript.
//
// The same call-content dedup guard as Upsert/UpsertTracked applies (issue
// #134, see callDupCheckQuery's doc comment): if an active call document
// with byte-identical content already exists under a DIFFERENT source_id,
// this returns (false, ErrDuplicateTranscript) without writing — this is the
// guard that protects against the SAME audio being merged under two
// different SourceIDs (e.g. a duplicate or renamed recording file).
//
// Returns contentChanged exactly like UpsertTracked, for callers (the
// scheduler) that skip chunk/embedding regeneration when content did not
// actually change — rare here (a successful transcription produces new
// content by definition), but it keeps a re-run against the same audio
// idempotent rather than repeatedly re-chunking.
func (s *DocumentStore) AttachTranscript(ctx context.Context, doc *model.Document) (contentChanged bool, err error) {
	checkSourceTypeGuard(doc)

	var exists int
	qErr := s.pg.pool.QueryRow(ctx, callDupCheckQuery,
		doc.Content,
		doc.SourceID,
	).Scan(&exists)
	switch {
	case qErr == nil:
		slog.Info("store: skipping duplicate call content (attach-transcript)",
			"source_id", doc.SourceID,
			"content_len", len(doc.Content),
		)
		return false, ErrDuplicateTranscript
	case isNoRows(qErr):
		// No duplicate — proceed with the merge/insert.
	default:
		return false, fmt.Errorf("call dup check: %w", qErr)
	}

	meta, err := json.Marshal(doc.Metadata)
	if err != nil {
		return false, fmt.Errorf("marshal metadata: %w", err)
	}

	var embeddingArg interface{}
	if len(doc.Embedding) > 0 {
		embeddingArg = pgvector.NewVector(doc.Embedding)
	}

	// embedding_version: 빈 문자열이면 NULL 을 보내고, SQL 쪽에서 기존 값을
	// 유지한다(마이그레이션 037 참고). 임베딩을 만들지 않은 재수집 upsert 가
	// 이미 기록된 버전을 지우면 안 되기 때문이다.
	var embeddingVersionArg interface{}
	if doc.EmbeddingVersion != "" {
		embeddingVersionArg = doc.EmbeddingVersion
	}

	// Change detection mirrors UpsertTracked's CTE pattern — see that
	// method's doc comment for why EXCLUDED cannot be referenced in RETURNING
	// and why documents.content in RETURNING would already hold the
	// post-update value.
	const q = `
		WITH prev AS (
			SELECT content AS old_content
			FROM documents
			WHERE source_type = $1 AND source_id = $2
		)
		INSERT INTO documents
			(source_type, source_id, title, content, metadata, embedding, occurred_at, collected_at, embedding_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (source_type, source_id) DO UPDATE SET
			content      = EXCLUDED.content,
			title        = CASE WHEN EXCLUDED.metadata->>'pii_name_redacted' = 'true' THEN EXCLUDED.title ELSE documents.title END,
            metadata     = CASE WHEN EXCLUDED.metadata->>'pii_name_redacted' = 'true'
                           THEN (documents.metadata - 'contact_name' - 'number') || EXCLUDED.metadata
                           ELSE documents.metadata || EXCLUDED.metadata END,
            title_summary = CASE WHEN EXCLUDED.metadata->>'pii_name_redacted' = 'true' THEN NULL ELSE documents.title_summary END,
            bullet_summary = CASE WHEN EXCLUDED.metadata->>'pii_name_redacted' = 'true' THEN NULL ELSE documents.bullet_summary END,
            summary_embedding = CASE WHEN EXCLUDED.metadata->>'pii_name_redacted' = 'true' THEN NULL ELSE documents.summary_embedding END,
			embedding    = CASE WHEN EXCLUDED.metadata->>'pii_name_redacted' = 'true' THEN EXCLUDED.embedding ELSE COALESCE(EXCLUDED.embedding, documents.embedding) END,
			embedding_version = CASE WHEN EXCLUDED.metadata->>'pii_name_redacted' = 'true' OR EXCLUDED.embedding IS NOT NULL
			                    THEN EXCLUDED.embedding_version
			                    ELSE documents.embedding_version END,
			occurred_at  = COALESCE(documents.occurred_at, EXCLUDED.occurred_at),
			collected_at = EXCLUDED.collected_at,
			status       = 'active',
			deleted_at   = NULL,
			updated_at   = now()
		RETURNING id, created_at, updated_at,
		          (xmax::text::bigint = 0) AS was_insert,
		          (COALESCE((SELECT old_content FROM prev), '') IS DISTINCT FROM $4
                   OR COALESCE(($5::jsonb)->>'pii_name_redacted' = 'true', false)) AS content_changed`

	var wasInsert bool
	row := s.pg.pool.QueryRow(ctx, q,
		doc.SourceType,
		doc.SourceID,
		doc.Title,
		doc.Content,
		meta,
		embeddingArg,
		doc.OccurredAt,
		doc.CollectedAt,
		embeddingVersionArg,
	)
	if err := row.Scan(&doc.ID, &doc.CreatedAt, &doc.UpdatedAt, &wasInsert, &contentChanged); err != nil {
		return false, err
	}
	if wasInsert {
		contentChanged = true
	}
	return contentChanged, nil
}

// GetByID retrieves a single document by primary key.
func (s *DocumentStore) GetByID(ctx context.Context, id uuid.UUID) (*model.Document, error) {
	const q = `
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding
		FROM documents WHERE id = $1`

	row := s.pg.pool.QueryRow(ctx, q, id)
	doc, err := scanDocument(row)
	if err != nil {
		return nil, fmt.Errorf("get document %s: %w", id, err)
	}
	return doc, nil
}

// ListBySource returns active documents of a given source type, ordered by the
// original event time (occurred_at) when available, falling back to collected_at
// for rows that have no event-time concept. NULLS LAST ensures untagged rows
// appear after all event-timestamped rows.
// When src is empty, all active documents are returned regardless of source type.
func (s *DocumentStore) ListBySource(ctx context.Context, src model.SourceType, limit, offset int) ([]*model.Document, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if src == "" {
		const q = `
			SELECT id, source_type, source_id, title, content, metadata, embedding,
			       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
			       title_summary, bullet_summary, summary_embedding
			FROM documents
			WHERE status = 'active'
			ORDER BY COALESCE(occurred_at, collected_at) DESC
			LIMIT $1 OFFSET $2`
		rows, err = s.pg.pool.Query(ctx, q, limit, offset)
		if err != nil {
			return nil, fmt.Errorf("list documents: %w", err)
		}
	} else {
		const q = `
			SELECT id, source_type, source_id, title, content, metadata, embedding,
			       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
			       title_summary, bullet_summary, summary_embedding
			FROM documents
			WHERE source_type = $1
			  AND status = 'active'
			ORDER BY COALESCE(occurred_at, collected_at) DESC
			LIMIT $2 OFFSET $3`
		rows, err = s.pg.pool.Query(ctx, q, src, limit, offset)
		if err != nil {
			return nil, fmt.Errorf("list by source %q: %w", src, err)
		}
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// ListRecent returns active documents ordered by the original event time
// (occurred_at) when available, falling back to collected_at for rows that
// have no event-time concept. This ensures "most recent" reflects when the
// underlying event actually happened (email sent, call placed, etc.) rather
// than when second-brain ingested the document.
//
// When includeSrc is non-empty, only documents of that source type are returned.
// excludeSrcs lists source types to omit from results; it is applied after
// includeSrc and may be empty.
func (s *DocumentStore) ListRecent(ctx context.Context, includeSrc model.SourceType, excludeSrcs []model.SourceType, limit, offset int) ([]*model.Document, error) {
	args := []interface{}{}

	var whereClauses []string
	whereClauses = append(whereClauses, "status = 'active'")

	if includeSrc != "" {
		args = append(args, includeSrc)
		whereClauses = append(whereClauses, fmt.Sprintf("source_type = $%d", len(args)))
	}

	if len(excludeSrcs) > 0 {
		args = append(args, excludeSrcs)
		whereClauses = append(whereClauses, fmt.Sprintf("source_type <> ALL($%d)", len(args)))
	}

	args = append(args, limit, offset)
	limitIdx := len(args) - 1
	offsetIdx := len(args)

	where := ""
	for i, clause := range whereClauses {
		if i == 0 {
			where = "WHERE " + clause
		} else {
			where += "\n  AND " + clause
		}
	}

	q := fmt.Sprintf(`
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding
		FROM documents
		%s
		ORDER BY COALESCE(occurred_at, collected_at) DESC
		LIMIT $%d OFFSET $%d`, where, limitIdx, offsetIdx)

	rows, err := s.pg.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list recent documents: %w", err)
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// Search performs hybrid search using full-text and, when an embedding is
// provided, vector cosine similarity. Results are combined via Reciprocal
// Rank Fusion (RRF).
func (s *DocumentStore) Search(ctx context.Context, query model.SearchQuery) ([]*model.SearchResult, error) {
	if query.Limit <= 0 {
		query.Limit = 20
	}

	if len(query.Embedding) > 0 {
		return s.hybridSearch(ctx, query)
	}
	return s.fulltextSearch(ctx, query)
}

// sortOrder returns the ORDER BY clause for search queries.
// Only the whitelisted literal "recent" changes the order; all other values
// (including "" and "relevance") fall back to relevance (score DESC).
// This whitelist comparison prevents SQL injection.
//
// tableAlias is the SQL table alias used in the calling query ("d" for hybrid
// search which aliases documents as d, "" for fulltext search which uses the
// bare column names). When tableAlias is non-empty a dot-prefix is added.
//
// "recent" means NEAREST TO NOW FIRST, and which SQL clause expresses that
// depends on the event-time window. This function does NOT decide that: both
// the whitelist test and the direction come from model.SearchQuery
// (SortsByRecency / RecencyAscending), which is the single definition of the
// rule and carries its full rationale.
//
// It has to be shared rather than restated because this clause is not the only
// place the order is applied: internal/search re-establishes the same order in
// Go over result sets it assembled itself (store results fused with chunk-lane
// candidates, which never passed through this ORDER BY). Two copies of the rule
// would order those two result shapes differently, and nothing in the response
// distinguishes them.
//
// The historical DESC branch keeps its COALESCE onto collected_at — the same
// strategy as ListRecent / ListBySource, so "latest gmail" returns the most
// recently sent email rather than the most recently ingested one.
//
// The ASC branch does not COALESCE. Whenever either bound is set the lanes
// already exclude occurred_at IS NULL (see appendOccurredRangeFilters), so on
// this branch occurred_at is non-NULL by construction and coalescing would only
// re-admit collected_at as a sort key for rows that cannot be in the result.
//
// Injection safety is unchanged and must stay that way: `sort` is compared
// against the exact literal "recent" and never reaches the output, the window
// is read for its direction only (its values are bound as parameters
// elsewhere), and every returned clause remains a compile-time constant modulo
// the caller-supplied alias. A non-whitelisted Sort still collapses to
// "score DESC" regardless of the window.
func sortOrder(query model.SearchQuery, now time.Time, tableAlias string) string {
	if !query.SortsByRecency() {
		return "score DESC"
	}

	prefix := ""
	if tableAlias != "" {
		prefix = tableAlias + "."
	}

	if query.RecencyAscending(now) {
		return fmt.Sprintf("%soccurred_at ASC", prefix)
	}
	return fmt.Sprintf("COALESCE(%soccurred_at, %scollected_at) DESC", prefix, prefix)
}

// fulltextSearch uses PostgreSQL ts_rank against the pre-computed tsvector column.
// pg_bigm LIKE matching is added as an OR condition so that Korean queries lacking
// morphological tsvector coverage are still retrieved via 2-gram index.
func (s *DocumentStore) fulltextSearch(ctx context.Context, query model.SearchQuery) ([]*model.SearchResult, error) {
	q, args := buildFulltextSearchQuery(query)

	rows, err := s.pg.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("fulltext search: %w", err)
	}
	defer rows.Close()

	return collectResults(rows, "fulltext")
}

// buildFulltextSearchQuery renders the non-vector search statement and its
// positional arguments. Split out of fulltextSearch (which only executes it) so
// the WHERE predicates — status, source type, and the occurred_at window — can
// be asserted without a database, the same way buildEntityCTE is.
func buildFulltextSearchQuery(query model.SearchQuery) (string, []interface{}) {
	args := []interface{}{query.Query, query.Limit}

	statusFilter := "AND status = 'active'"
	if query.IncludeDeleted {
		statusFilter = ""
	}

	// Include filter: set membership over the effective include set (see
	// model.SearchQuery.IncludeSourceTypes), bound as ONE parameter. The list
	// is never interpolated — every fragment in this file is assembled with
	// fmt.Sprintf, and that mechanism has already caused one production
	// incident here.
	sourceFilter := ""
	if include := query.IncludeSourceTypes(); len(include) > 0 {
		sourceFilter = fmt.Sprintf("AND source_type = ANY($%d)", len(args)+1)
		args = append(args, include)
	}

	excludeFilter := ""
	if len(query.ExcludeSourceTypes) > 0 {
		excludeFilter = fmt.Sprintf("AND source_type <> ALL($%d)", len(args)+1)
		args = append(args, query.ExcludeSourceTypes)
	}

	// Retention filter (documents.metadata->>'retention'). NULL-safe via
	// COALESCE so a document with no "retention" key at all — most of the
	// corpus — is never excluded; see appendRetentionFilter's doc comment for
	// the full rationale (shared verbatim with buildHybridSearchQuery).
	var retentionFilter string
	args, retentionFilter, _ = appendRetentionFilter(args, query.ExcludeRetention)

	// Event-time window, applied in the WHERE clause for the same reason as in
	// hybridSearch: this statement is capped by LIMIT $2, so the window has to
	// constrain what is selected, not what is returned after selection.
	var occurredFilter string
	args, occurredFilter, _ = appendOccurredRangeFilters(args, query.OccurredFrom, query.OccurredTo)

	// 질의 형태(#276). raw(키워드 없음)의 두 조각은 #276 이전 SQL 과 글자
	// 하나까지 같다. 키워드가 있으면 접두 OR tsquery 와 키워드별 LIKE 로
	// 바꾼다 — 인자는 다른 모든 인자 뒤에 붙으므로 기존 번호는 움직이지 않는다.
	scoreExpr := `GREATEST(
		           ts_rank(tsv, plainto_tsquery('simple', $1)),
		           ts_rank(tsv, plainto_tsquery('english', $1))
		       )`
	matchExpr := `(tsv @@ plainto_tsquery('simple', $1)
		   OR tsv @@ plainto_tsquery('english', $1)
		   OR content LIKE '%' || $1 || '%'
		   OR title   LIKE '%' || $1 || '%'
		   OR (source_type = 'call' AND strpos(lower(metadata->>'contact_name'), lower($1)) > 0))`
	if query.SparseTerms.Active() {
		var sp sparseSQL
		args, sp = appendSparseTerms(args, query.SparseTerms)
		// 세 번째 항: LIKE 로만 맞은 문서도 맞은 키워드 비율과 질문 전체
		// 일치로 순서가 서게 한다(청크 레인과 같은 0.01 척도 — 진짜 FTS
		// 히트보다는 항상 아래). raw 에서 LIKE 만 맞은 문서는 전부 0점이라
		// 서로 순서가 없었다. 이 항이 $1 을 참조하는 것도 필요하다: 참조되지
		// 않는 파라미터는 PostgreSQL 이 타입을 정하지 못해 질의가 실패한다.
		scoreExpr = fmt.Sprintf("GREATEST(\n\t\t           %s,\n\t\t           %s,\n\t\t           %s\n\t\t       )",
			sp.tsRank("tsv", "simple"), sp.tsRank("tsv", "english"), sp.docSparseRank())
		matchExpr = fmt.Sprintf("(%s\n\t\t   OR %s\n\t\t   OR %s)",
			sp.tsMatch("tsv", "simple"), sp.tsMatch("tsv", "english"), sp.likeAny(true, "content", "title"))
	}

	// The LIKE pattern uses SQL string concatenation ('%' || $1 || '%') so that
	// pg_bigm's gin_bigm_ops index is used automatically. 매칭·점수 식은 위의
	// scoreExpr/matchExpr 로 옮겨 %s 인자로 넣으므로 그 안의 '%' 는 포맷
	// 문자열로 해석되지 않는다.
	q := fmt.Sprintf(`
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding,
		       %s AS score
		FROM documents
		WHERE %s
		%s
		%s
		%s
		%s
		%s
		ORDER BY %s
		LIMIT $2`, scoreExpr, matchExpr, statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredFilter, sortOrder(query, time.Now(), ""))

	return q, args
}

// appendOccurredRangeFilters binds the caller's event-time window to args and
// returns the matching WHERE fragments in both the unqualified form (for the
// CTEs that scan `documents` directly) and the `d.`-qualified form (for the
// entity lane, which joins it). Both forms reference the SAME positional
// parameters, so the two must always be produced by this one call.
//
// Three properties are deliberate and load-bearing:
//
//   - The bounds are bound as parameters, never interpolated. Every other
//     fragment in this file is assembled with fmt.Sprintf; a timestamp spliced
//     into the string would be an injection surface and a parse hazard.
//   - The comparison is against occurred_at alone, not
//     COALESCE(occurred_at, collected_at). A large share of the corpus has a
//     NULL occurred_at, and coalescing would reinterpret "ingested during the
//     window" as "happened during the window" — precisely the noise the window
//     exists to remove. Rows with a NULL occurred_at are therefore excluded,
//     which the SQL gets for free: a comparison against NULL is never true.
//   - The window is half-open, [from, to). Consecutive windows tile without
//     overlap and a document at exactly `to` belongs only to the next one.
//
// Either bound may be nil; both nil returns empty fragments and args unchanged.
func appendOccurredRangeFilters(args []interface{}, from, to *time.Time) (newArgs []interface{}, plain, qualified string) {
	var plainParts, qualifiedParts []string

	if from != nil {
		p := len(args) + 1
		plainParts = append(plainParts, fmt.Sprintf("AND occurred_at >= $%d", p))
		qualifiedParts = append(qualifiedParts, fmt.Sprintf("AND d.occurred_at >= $%d", p))
		args = append(args, *from)
	}
	if to != nil {
		p := len(args) + 1
		plainParts = append(plainParts, fmt.Sprintf("AND occurred_at < $%d", p))
		qualifiedParts = append(qualifiedParts, fmt.Sprintf("AND d.occurred_at < $%d", p))
		args = append(args, *to)
	}

	const sep = "\n\t\t\t"
	return args, strings.Join(plainParts, sep), strings.Join(qualifiedParts, sep)
}

// appendRetentionFilter binds the caller's ExcludeRetention list to args and
// returns the matching WHERE fragment in both the unqualified form (for the
// four CTEs that scan `documents` directly, and for fulltextSearch's plain
// query) and the `d.`-qualified form (for the entity lane, which joins).
// Mirrors appendOccurredRangeFilters' shape and reasoning:
//
//   - The predicate coalesces a NULL retention tag to an empty string before
//     comparing, which is NULL-safe. A large majority of documents.metadata
//     rows have no "retention" key at all (only gmail has been segmented so
//     far, per model.RetentionTag's doc comment) — `metadata->>'retention'`
//     on such a row is SQL NULL, and NULL compared against anything with <>
//     is never true, which would silently EXCLUDE every untagged document.
//     The coalesced empty string is never itself one of
//     RetentionKeep/RetentionLow/RetentionDisposable, so an untagged row
//     always survives the <> ALL(...) test regardless of what is in the
//     exclude list.
//   - The list travels as ONE bound parameter, never interpolated — every
//     fragment in this file is built with fmt.Sprintf, and an interpolated
//     list here would reopen the exact injection surface the source-type and
//     occurred_at filters were written to avoid.
//   - No index exists on metadata->>'retention', and none is added by this
//     change. That is a deliberate no-op, not an oversight: every lane this
//     filter reaches already narrows the candidate set to at most `LIMIT $3`
//     rows (or `LIMIT $2` for fulltextSearch) via its own indexed access path
//     — the GIN tsvector index for fts, the HNSW index for vec, pg_bigm's GIN
//     index for bigm, the entity join, or (for fulltextSearch) the same
//     tsvector/pg_bigm OR-clause. PostgreSQL therefore evaluates this
//     COALESCE/<>ALL as an extra Filter condition on that already-bounded row
//     set, exactly like the pre-existing `status = 'active'` filter — it
//     cannot change which index scan the planner picks, and at ≤ a few
//     hundred candidate rows per lane the per-row jsonb text extraction is
//     immaterial next to the ANN/GIN scan cost it rides alongside. No EXPLAIN
//     regression is expected for the same reason status/source_type already
//     don't need their own index: the filter never becomes the plan's driving
//     predicate, it only trims candidates the driving predicate already found.
//
// An empty exclude list returns empty fragments and args unchanged — this is
// the common case for any request that used IncludeRetention to opt out of
// the default (search.applyRetentionExclusionDefault).
func appendRetentionFilter(args []interface{}, exclude []string) (newArgs []interface{}, plain, qualified string) {
	if len(exclude) == 0 {
		return args, "", ""
	}
	p := len(args) + 1
	plain = fmt.Sprintf("AND COALESCE(metadata->>'retention', '') <> ALL($%d)", p)
	qualified = fmt.Sprintf("AND COALESCE(d.metadata->>'retention', '') <> ALL($%d)", p)
	return append(args, exclude), plain, qualified
}

// buildOccurredRangeIDQuery renders the statement that narrows a candidate set
// of document IDs to those whose occurred_at falls inside [from, to), and the
// bound arguments for the window. The ID list itself is bound as $1 by the
// caller, so the returned args start at $2.
//
// Split out for the same reason as the other builders in this file: the WHERE
// clause is the whole content of the function, and it is the only thing worth
// asserting without a database.
//
// It reuses appendOccurredRangeFilters so the window semantics are defined
// exactly once. That matters more here than anywhere else: this statement
// verifies candidates that a DIFFERENT lane produced. If its notion of the
// window drifted from the lanes' — half-open vs closed, or a COALESCE onto
// collected_at — the two halves of one result set would be answering different
// questions, and the difference would be invisible in the response.
func buildOccurredRangeIDQuery(from, to *time.Time) (string, []interface{}) {
	// One placeholder is already consumed by the ID list ($1).
	args, plain, _ := appendOccurredRangeFilters([]interface{}{nil}, from, to)
	return fmt.Sprintf(`
		SELECT id
		FROM documents
		WHERE id = ANY($1)
		%s`, plain), args[1:]
}

// FilterIDsByOccurredRange returns the subset of ids whose documents.occurred_at
// falls inside the half-open window [from, to). Rows with a NULL occurred_at
// never survive a window — the SQL comparison gives that for free, and it is
// the same rule every retrieval lane applies.
//
// This exists so that the search service's chunk lanes can honour an event-time
// window. Chunk rows carry no occurred_at, so before this the only safe
// behaviour was to switch those lanes off whenever a window was set, which
// deleted chunk-level recall from every temporal query. Verifying the candidate
// IDs against the documents table costs one primary-key lookup per windowed
// request (at most `limit` ids after per-document deduplication) and keeps both
// properties: chunk recall and a window the result provably satisfies.
//
// An empty ids slice short-circuits without touching the database.
func (s *DocumentStore) FilterIDsByOccurredRange(ctx context.Context, ids []uuid.UUID, from, to *time.Time) (map[uuid.UUID]struct{}, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]struct{}{}, nil
	}

	q, boundArgs := buildOccurredRangeIDQuery(from, to)
	args := make([]interface{}, 0, len(boundArgs)+1)
	args = append(args, ids)
	args = append(args, boundArgs...)

	rows, err := s.pg.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("filter ids by occurred range: %w", err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID]struct{}, len(ids))
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("filter ids by occurred range scan: %w", err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("filter ids by occurred range iter: %w", err)
	}
	return out, nil
}

// hybridSearch combines full-text and vector search ranks via RRF.
func (s *DocumentStore) hybridSearch(ctx context.Context, query model.SearchQuery) ([]*model.SearchResult, error) {
	w, err := s.resolveWeights(ctx, query)
	if err != nil {
		return nil, err
	}

	q, args := buildHybridSearchQuery(query, w)

	rows, err := s.pg.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("hybrid search: %w", err)
	}
	defer rows.Close()

	results, err := collectResults(rows, "hybrid")
	if err != nil {
		return nil, err
	}
	if len(results) > query.Limit {
		results = results[:query.Limit]
	}
	return results, nil
}

// resolveWeights turns the caller-supplied (possibly zero) weights into the
// effective per-lane weights, applying the two gates that need runtime state:
// the summary-vec coverage gate (a DB query) and the entity-lane env gate.
// Kept separate from buildHybridSearchQuery so that query construction stays a
// pure function of (query, weights) and is testable without a database.
//
// The error return is currently always nil — a failing coverage query degrades
// to "lane disabled" rather than failing the search — but is kept so a future
// hard failure has somewhere to go.
func (s *DocumentStore) resolveWeights(ctx context.Context, query model.SearchQuery) (model.SearchWeights, error) {
	w := query.Weights.Defaults()
	// Coverage gate: apply only when the caller has not explicitly set a weight
	// and has not explicitly disabled the summary-vec lane (#63).
	// - DisableSummaryVec=true → Defaults() already set w.SummaryVec=0.0; skip gate.
	// - SummaryVec>0 (explicit) → caller bypasses gate; use the value as-is.
	// - SummaryVec==0 (unset)   → apply gate: enable when corpus coverage is sufficient.
	if !query.Weights.DisableSummaryVec && query.Weights.SummaryVec == 0 {
		// Coverage gate: "decide based on corpus coverage".  Defaults() preserved
		// zero, so we resolve the effective weight here.
		//
		// - Coverage >= threshold → enable at DefaultSummaryVecWeight (0.8).
		// - Coverage <  threshold → disable (0.0) to prevent systematic demotion
		//   of un-summarised documents during backfill.
		// - Coverage query error  → disable (safe fallback; avoids bias on error).
		coverage, coverageErr := s.SummaryCoverageRatio(ctx)
		if coverageErr == nil && coverage >= model.SummaryVecCoverageThreshold() {
			w.SummaryVec = model.DefaultSummaryVecWeight
		} else {
			w.SummaryVec = 0.0
		}
	}

	// Entity lane gate (#139): enable when ENTITY_EXTRACTION_ENABLED env var is set.
	// EntityWeight<0 means explicit disable by caller; zero means "auto".
	// The lane is a no-op when entities aren't in the DB — FULL OUTER JOIN is safe.
	if w.EntityWeight == 0 {
		if entityExtractionEnabled() {
			w.EntityWeight = model.DefaultEntityWeight
		}
		// else: leave 0 — entity CTE contributes 0 to RRF score (no-op).
	} else if w.EntityWeight < 0 {
		w.EntityWeight = 0 // explicit disable
	}

	return w, nil
}

// buildHybridSearchQuery renders the five-lane RRF statement and its positional
// arguments for the given query and already-resolved weights.
//
// Split out of hybridSearch so every lane's WHERE predicates are assertable
// without a database. That matters most for filters that constrain the
// candidate pool: each lane is capped by LIMIT $3, so a filter missing from one
// lane does not merely widen that lane — it lets out-of-scope documents consume
// candidate slots and push in-scope documents out of the fused result entirely.
func buildHybridSearchQuery(query model.SearchQuery, w model.SearchWeights) (string, []interface{}) {
	args := []interface{}{
		query.Query,
		pgvector.NewVector(query.Embedding),
		query.Limit * 2, // fetch more candidates before RRF merge
	}

	// Each filter is built twice: once unqualified for the four CTEs that scan
	// `documents` directly, and once qualified for the entity CTE, which joins
	// `documents` rather than scanning it. Same positional parameters, same
	// semantics — only the column prefix differs. Keeping the pairs adjacent is
	// deliberate: the entity lane previously applied NONE of these filters and
	// the outer join re-applied nothing, so a document reachable only through
	// the entity lane bypassed both the soft-delete filter and every source
	// type exclusion, insight documents included.
	statusFilter, entityStatusFilter := "AND status = 'active'", "AND d.status = 'active'"
	if query.IncludeDeleted {
		statusFilter, entityStatusFilter = "", ""
	}

	// Include filter: set membership over the effective include set (see
	// model.SearchQuery.IncludeSourceTypes). One bound parameter, referenced by
	// all five lanes — a lane that omitted it would not merely widen itself, it
	// would spend candidate slots (LIMIT $3) on documents the caller excluded.
	sourceFilter, entitySourceFilter := "", ""
	if include := query.IncludeSourceTypes(); len(include) > 0 {
		p := len(args) + 1
		sourceFilter = fmt.Sprintf("AND source_type = ANY($%d)", p)
		entitySourceFilter = fmt.Sprintf("AND d.source_type = ANY($%d)", p)
		args = append(args, include)
	}

	excludeFilter, entityExcludeFilter := "", ""
	if len(query.ExcludeSourceTypes) > 0 {
		p := len(args) + 1
		excludeFilter = fmt.Sprintf("AND source_type <> ALL($%d)", p)
		entityExcludeFilter = fmt.Sprintf("AND d.source_type <> ALL($%d)", p)
		args = append(args, query.ExcludeSourceTypes)
	}

	// Retention filter (documents.metadata->>'retention'), one bound parameter
	// shared by all five lanes — see appendRetentionFilter for the NULL-safety
	// and no-new-index reasoning.
	var retentionFilter, entityRetentionFilter string
	args, retentionFilter, entityRetentionFilter = appendRetentionFilter(args, query.ExcludeRetention)

	// Event-time window. Like the source filters, this constrains the candidate
	// pool INSIDE every lane rather than reordering the fused result: each lane
	// is capped by LIMIT $3, so an out-of-window document that reaches a lane
	// consumes a candidate slot an in-window document needed. Sorting by
	// recency (sortOrder) cannot substitute — it only permutes what the lanes
	// already selected.
	var occurredFilter, entityOccurredFilter string
	args, occurredFilter, entityOccurredFilter = appendOccurredRangeFilters(args, query.OccurredFrom, query.OccurredTo)

	// RRF formula: w / (k + rank), where w is the per-signal weight and k
	// prevents very high scores for top-ranked results (standard k=60).
	// Four CTEs (fts, vec, bigm, summvec) are merged via FULL OUTER JOIN.
	// Each CTE shares the same statusFilter/sourceFilter/excludeFilter snippets;
	// args are appended once and referenced by the same positional parameters.
	// bigm uses pg_bigm's gin_bigm_ops index via LIKE '%%' || $1 || '%%'.
	// (#276 이후 이 조건은 아래 bigmMatch 변수에 있고 %s 인자로 들어가므로
	// 포맷 문자열을 거치지 않는다 — 출력이 이전과 같도록 '%%' 를 그대로 둔다.)
	// bigm lane ranks by GREATEST(bigm_similarity(content,$1), bigm_similarity(title,$1))
	// so Korean partial-match is ordered by real relevance, not document length (#138).
	// summvec uses the same query embedding ($2) as vec for consistency.
	// Weight parameters are injected as Go-formatted literals (not SQL params)
	// because they are floats under our control, never from user input.
	//
	// Coverage gate (#63) and entity gate (#139) are resolved by resolveWeights
	// before this function is called; w is the effective per-lane weighting.
	//
	// Build a normalised entity query: lower-cased, whitespace-trimmed tokens
	// joined by OR for the entity name LIKE match. This enables partial-name
	// matching (e.g. "홍길동" matches entity with normalized_name='홍길동').
	// The entity param position depends on how many args are already bound:
	// $4 when no source filter is active, $5 or $6 when source/exclude filters
	// push it further. The actual placeholder is computed via len(args)+1.
	entityFilterParam := ""
	if w.EntityWeight > 0 {
		entityFilterParam = fmt.Sprintf("$%d", len(args)+1)
		args = append(args, strings.ToLower(strings.TrimSpace(query.Query)))
	}

	// entityCTE is the SQL fragment for the entity lane. When the lane is
	// disabled (weight=0), we use a trivially-empty CTE to avoid syntax errors.
	entityCTE := emptyEntityCTE
	if w.EntityWeight > 0 {
		entityCTE = buildEntityCTE(entityFilterParam, entityStatusFilter, entitySourceFilter, entityExcludeFilter, entityRetentionFilter, entityOccurredFilter)
	}

	// 질의 형태(#276). 아래 네 조각의 기본값은 #276 이전 SQL 과 글자 하나까지
	// 같다. 키워드가 있으면 fts·bigm 레인만 바꾸고 vec·summvec·entity 레인은
	// 건드리지 않는다. 키워드 인자는 엔티티 파라미터까지 전부 붙인 뒤 맨
	// 끝에 붙인다 — 그래야 엔티티 CTE 가 바이트 단위로 그대로 남는다.
	ftsRank := `GREATEST(
			           ts_rank(tsv, plainto_tsquery('simple', $1)),
			           ts_rank(tsv, plainto_tsquery('english', $1))
			       )`
	ftsMatch := `(tsv @@ plainto_tsquery('simple', $1)
			   OR tsv @@ plainto_tsquery('english', $1))`
	bigmOrderPrefix := ""
	bigmMatch := `(content LIKE '%%' || $1 || '%%'
			    OR title   LIKE '%%' || $1 || '%%'
			    OR (source_type = 'call' AND strpos(lower(metadata->>'contact_name'), lower($1)) > 0))`
	if query.SparseTerms.Active() {
		var sp sparseSQL
		args, sp = appendSparseTerms(args, query.SparseTerms)
		ftsRank = fmt.Sprintf("GREATEST(\n\t\t\t           %s,\n\t\t\t           %s\n\t\t\t       )",
			sp.tsRank("tsv", "simple"), sp.tsRank("tsv", "english"))
		ftsMatch = fmt.Sprintf("(%s\n\t\t\t   OR %s)", sp.tsMatch("tsv", "simple"), sp.tsMatch("tsv", "english"))
		// bigm 레인 순서: 맞은 키워드 수 → 질문 전체 일치 보너스(기존의 정밀
		// 일치가 동점일 때 이기게) → 기존 bigm_similarity → id.
		bigmOrderPrefix = fmt.Sprintf("\n\t\t\t           %s DESC,\n\t\t\t           %s DESC,",
			sp.likeCount(true, "content", "title"),
			"CASE WHEN content LIKE '%' || $1 || '%' OR title LIKE '%' || $1 || '%' THEN 1 ELSE 0 END")
		bigmMatch = sp.likeAny(true, "content", "title")
	}

	q := fmt.Sprintf(`
		WITH fts AS (
			SELECT id,
			       row_number() OVER (ORDER BY %s DESC) AS rank
			FROM documents
			WHERE %s
			%s
			%s
			%s
			%s
			%s
			LIMIT $3
		),
		vec AS (
			SELECT id,
			       row_number() OVER (ORDER BY embedding <=> $2 ASC) AS rank
			FROM documents
			WHERE embedding IS NOT NULL
			%s
			%s
			%s
			%s
			%s
			LIMIT $3
		),
		bigm AS (
			SELECT id,
			       row_number() OVER (ORDER BY%s
			           GREATEST(
			               bigm_similarity(content, $1),
			               bigm_similarity(title,   $1),
			               CASE WHEN source_type = 'call' THEN bigm_similarity(coalesce(metadata->>'contact_name', ''), $1) ELSE 0 END
			           ) DESC, id ASC) AS rank
			FROM documents
			WHERE %s
			%s
			%s
			%s
			%s
			%s
			LIMIT $3
		),
		summvec AS (
			SELECT id,
			       row_number() OVER (ORDER BY summary_embedding <=> $2 ASC) AS rank
			FROM documents
			WHERE summary_embedding IS NOT NULL
			%s
			%s
			%s
			%s
			%s
			LIMIT $3
		),
		%s,
		rrf AS (
			SELECT
				COALESCE(fts.id, vec.id, bigm.id, summvec.id, entity.id) AS id,
				%s AS score
			FROM fts
			FULL OUTER JOIN vec     ON fts.id = vec.id
			FULL OUTER JOIN bigm    ON COALESCE(fts.id, vec.id) = bigm.id
			FULL OUTER JOIN summvec ON COALESCE(fts.id, vec.id, bigm.id) = summvec.id
			FULL OUTER JOIN entity  ON COALESCE(fts.id, vec.id, bigm.id, summvec.id) = entity.id
		)
		SELECT d.id, d.source_type, d.source_id, d.title, d.content, d.metadata,
		       d.embedding, d.status, d.deleted_at, d.occurred_at, d.collected_at, d.created_at, d.updated_at,
		       d.title_summary, d.bullet_summary, d.summary_embedding,
		       rrf.score
		FROM rrf
		JOIN documents d ON d.id = rrf.id
		ORDER BY %s
		LIMIT $3`,
		ftsRank, ftsMatch, // fts: 질의 형태(#276)
		statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredFilter, // fts
		statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredFilter, // vec
		bigmOrderPrefix, bigmMatch, // bigm: 질의 형태(#276)
		statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredFilter, // bigm
		statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredFilter, // summvec
		entityCTE, // entity lane carries the same filters in d.-qualified form
		buildRRFScoreExpr(w),
		sortOrder(query, time.Now(), "d"))

	return q, args
}

// buildRRFScoreExpr renders the weighted-sum SQL expression that fuses the
// five per-lane ranks (fts, vec, bigm, summvec, entity) into a single RRF
// relevance score.
//
// Every weight/k value is explicitly cast to ::float8. Go's "%g" verb formats
// a whole-number float64 without a decimal point (1.0 -> "1", 60.0 -> "60"),
// and the default weights (FTS/Vec/Bigm=1.0, RRFK=60.0) all format this way.
// Without the cast, PostgreSQL parses "1/(60 + fts.rank)" as `integer`
// division: since rank is always >= 1, the denominator is always >= 61,
// strictly greater than the numerator, so the result truncates to 0 for every
// row. That collapsed the fts/vec/bigm lanes' contribution to `score` to zero
// on every hybridSearch call, leaving the final ORDER BY with no real
// relevance signal for the vast majority of the corpus.
func buildRRFScoreExpr(w model.SearchWeights) string {
	return fmt.Sprintf(
		`COALESCE(%g::float8/(%g::float8 + fts.rank),     0)
				+ COALESCE(%g::float8/(%g::float8 + vec.rank),     0)
				+ COALESCE(%g::float8/(%g::float8 + bigm.rank),    0)
				+ COALESCE(%g::float8/(%g::float8 + summvec.rank), 0)
				+ COALESCE(%g::float8/(%g::float8 + entity.rank),  0)`,
		w.FTSWeight, w.RRFK,
		w.VecWeight, w.RRFK,
		w.BigmWeight, w.RRFK,
		w.SummaryVec, w.RRFK,
		w.EntityWeight, w.RRFK,
	)
}

// emptyEntityCTE is the entity lane when its RRF weight is zero: a
// trivially-empty CTE, so the FULL OUTER JOIN below stays syntactically valid
// and contributes nothing to the score.
const emptyEntityCTE = `entity AS (SELECT NULL::uuid AS id, NULL::bigint AS rank WHERE false)`

// buildEntityCTE renders the entity RRF lane (#139).
//
// It joins `documents` purely so the status/source filters can be applied
// INSIDE the lane. Applying them here rather than at the outer join matters:
// the CTE is capped by `LIMIT $3`, so filtering afterwards would let excluded
// documents consume candidate slots and silently shrink the entity lane's
// contribution.
//
// The filter fragments must be `d.`-qualified — they are the entityStatusFilter
// / entitySourceFilter / entityExcludeFilter / entityRetentionFilter variants
// built in hybridSearch, not the unqualified ones used by the
// fts/vec/bigm/summvec CTEs. They bind the same positional parameters.
//
// Split out of hybridSearch so the filters can be asserted without a database:
// the lane's previous unfiltered form let soft-deleted and excluded documents
// (including insights) into results whenever they were reachable by entity
// name alone, and nothing in the test suite could observe it.
func buildEntityCTE(entityFilterParam, statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredRangeFilter string) string {
	return fmt.Sprintf(`entity AS (
			SELECT de.document_id AS id,
			       row_number() OVER (ORDER BY COUNT(*) DESC, de.document_id ASC) AS rank
			FROM document_entities de
			JOIN entities e ON e.id = de.entity_id
			JOIN documents d ON d.id = de.document_id
			WHERE e.normalized_name LIKE '%%%%' || %s || '%%%%'
			%s
			%s
			%s
			%s
			%s
			GROUP BY de.document_id
			LIMIT $3
		)`, entityFilterParam, statusFilter, sourceFilter, excludeFilter, retentionFilter, occurredRangeFilter)
}

// entityExtractionEnabled reports whether entity extraction has been enabled
// via the ENTITY_EXTRACTION_ENABLED environment variable (true / 1 / yes).
// This mirrors the check in cmd/collector/main.go so that the entity RRF lane
// automatically activates when the feature is running in the same deployment.
func entityExtractionEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("ENTITY_EXTRACTION_ENABLED")))
	return v == "true" || v == "1" || v == "yes"
}

// RecordCollectionLog writes a collection event to the collection_log table.
func (s *DocumentStore) RecordCollectionLog(ctx context.Context, sourceType model.SourceType, started time.Time, count int, collErr error) error {
	var errStr *string
	if collErr != nil {
		msg := collErr.Error()
		errStr = &msg
	}
	_, err := s.pg.pool.Exec(ctx, `
		INSERT INTO collection_log (source_type, started_at, finished_at, documents_count, error)
		VALUES ($1, $2, now(), $3, $4)`,
		sourceType, started, count, errStr,
	)
	return err
}

// LastCollectedAt returns the last collection watermark for the given
// (instance_id, source_type) pair, or the fallback when no row exists yet.
//
// Per-instance state decouples collectors that share a source_type (e.g.,
// filesystem scans on laptop, host1, host2) so one instance's recent scan
// cannot suppress older files seen by another instance.
func (s *DocumentStore) LastCollectedAt(ctx context.Context, instanceID string, src model.SourceType, fallback time.Time) time.Time {
	var t time.Time
	err := s.pg.pool.QueryRow(ctx,
		`SELECT last_collected_at FROM collector_state
		 WHERE instance_id = $1 AND source_type = $2`,
		instanceID, src,
	).Scan(&t)
	if err != nil || t.IsZero() {
		return fallback
	}
	return t
}

// UpdateCollectorState upserts the watermark for (instance_id, source_type).
// Callers should only invoke this on a successful collection cycle so that
// failed runs are retried from the previous watermark on the next tick.
func (s *DocumentStore) UpdateCollectorState(ctx context.Context, instanceID string, src model.SourceType, lastCollectedAt time.Time) error {
	_, err := s.pg.pool.Exec(ctx, `
		INSERT INTO collector_state (instance_id, source_type, last_collected_at, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (instance_id, source_type) DO UPDATE SET
			last_collected_at = EXCLUDED.last_collected_at,
			updated_at        = now()`,
		instanceID, src, lastCollectedAt,
	)
	if err != nil {
		return fmt.Errorf("update collector state (%s/%s): %w", instanceID, src, err)
	}
	return nil
}

// markDeletedEmptyGuardComment documents the belt-and-suspenders empty-slice
// guard added to MarkDeleted. An empty activeIDs slice passed to the Postgres
// query `source_id != ALL('{}')` evaluates as vacuously TRUE, which would
// soft-delete every active document for the source type — a silent data-loss
// path (see GitHub issue #135). The early-return no-op below prevents this.
// Callers (scheduler Layer 2) also guard against this via error propagation and
// deletion-ratio sanity checks; this guard is an additional safety net.
const markDeletedEmptyGuardComment = "empty activeIDs early-return no-op guard"

// MarkDeleted marks documents as deleted for source IDs not present in activeIDs.
// Only documents with status 'active' are updated. Returns the number of rows updated.
//
// Belt-and-suspenders: if activeIDs is empty, this function returns (0, nil)
// immediately without touching the database. An empty slice fed to the Postgres
// expression `source_id != ALL('{}')` is vacuously TRUE — it would delete every
// active document for the source type. The legitimate callers that intend a full
// wipe (if any ever exist) must use a dedicated, explicitly-named method rather
// than relying on an empty-slice side-effect.
func (s *DocumentStore) MarkDeleted(ctx context.Context, sourceType model.SourceType, activeIDs []string) (int, error) {
	if len(activeIDs) == 0 {
		// Empty slice guard: do NOT execute the query. See markDeletedEmptyGuardComment.
		return 0, nil
	}

	tag, err := s.pg.pool.Exec(ctx, `
		UPDATE documents
		SET status = 'deleted', deleted_at = now()
		WHERE source_type = $1
		  AND status = 'active'
		  AND source_id != ALL($2)`,
		sourceType, activeIDs,
	)
	if err != nil {
		return 0, fmt.Errorf("mark deleted for %s: %w", sourceType, err)
	}
	return int(tag.RowsAffected()), nil
}

// SoftDeleteBySourceID soft-deletes the active document identified by
// (source_type, source_id), if one exists, and reports whether a row was
// affected.
//
// Unlike Upsert/UpsertTracked, this method NEVER inserts: when no row with
// the given (source_type, source_id) exists, it is a pure no-op (false, nil).
// This is deliberate — it lets a collector that only observes a partial
// event stream (e.g. a calendar collector that receives a cancellation for
// an event it may never have fetched while active) signal a deletion
// without ever manufacturing a new document out of a tombstone event. It is
// idempotent: calling it again for an already-deleted or never-existing
// document is a harmless no-op (0 rows affected, no error).
func (s *DocumentStore) SoftDeleteBySourceID(ctx context.Context, sourceType model.SourceType, sourceID string) (bool, error) {
	const q = `
		UPDATE documents
		SET status = 'deleted', deleted_at = now()
		WHERE source_type = $1 AND source_id = $2 AND status = 'active'`
	tag, err := s.pg.pool.Exec(ctx, q, sourceType, sourceID)
	if err != nil {
		return false, fmt.Errorf("soft delete %s/%s: %w", sourceType, sourceID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// CountActiveDocuments returns the total number of active (non-deleted) documents
// for the given source type. It is used by the scheduler's deletion-ratio sanity
// check (issue #135) to detect a suspicious bulk-deletion before calling MarkDeleted.
// Implements scheduler.ActiveDocumentCounter.
func (s *DocumentStore) CountActiveDocuments(ctx context.Context, sourceType model.SourceType) (int, error) {
	var n int
	err := s.pg.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM documents
		WHERE source_type = $1 AND status = 'active'`,
		sourceType,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active documents for %s: %w", sourceType, err)
	}
	return n, nil
}

// CountBySource returns the number of active documents grouped by logical source.
// Deleted documents are excluded. The returned map is keyed by logical source string.
//
// Legacy documents stored under source_type='secretary' are re-bucketed by their
// metadata 'kind' field (e.g. "sms", "gmail", "call-log", "call-transcript",
// "calendar"). This means historical SMS documents count toward the "sms" bucket
// alongside post-cutover documents — there is no double-counting because the two
// sets are date-disjoint. All other source_type values are reported as-is.
func (s *DocumentStore) CountBySource(ctx context.Context) (map[string]int, error) {
	const q = `
		SELECT
		  CASE
		    WHEN source_type = 'secretary'
		      THEN COALESCE(NULLIF(metadata->>'kind', ''), 'secretary')
		    ELSE source_type
		  END AS logical_source,
		  COUNT(*)::bigint
		FROM documents
		WHERE status = 'active'
		GROUP BY logical_source`

	rows, err := s.pg.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("count by source: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int, 4)
	for rows.Next() {
		var src string
		var n int64
		if err := rows.Scan(&src, &n); err != nil {
			return nil, fmt.Errorf("count by source scan: %w", err)
		}
		out[src] = int(n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count by source iter: %w", err)
	}
	return out, nil
}

// CountPendingTranscription returns the number of active call documents
// (source_type='call') whose metadata.transcription is still "pending" —
// i.e. a recording was captured (ingest_recording.go) but WhisperCollector
// has not yet merged the transcript in via AttachTranscript. Surfaced on
// GET /api/v1/stats so a stuck whisper pipeline (offline server, exhausted
// disk, etc.) is visible without querying the database directly.
func (s *DocumentStore) CountPendingTranscription(ctx context.Context) (int, error) {
	var n int
	err := s.pg.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM documents
		WHERE source_type = 'call'
		  AND status      = 'active'
		  AND metadata ->> 'transcription' = 'pending'`,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending transcription: %w", err)
	}
	return n, nil
}

// --- baseline stats ---

// ContentLengthStats holds percentile and aggregate statistics for content length.
type ContentLengthStats struct {
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	Max  int64   `json:"max"`
}

// DocumentSourceStats holds per-source aggregate document metrics.
type DocumentSourceStats struct {
	Count         int                `json:"count"`
	ContentLength ContentLengthStats `json:"content_length"`
}

// BaselineDocumentStats aggregates document-level baseline metrics.
type BaselineDocumentStats struct {
	Total    int                            `json:"total"`
	BySource map[string]DocumentSourceStats `json:"by_source_type"`
}

// BaselineChunkStats aggregates chunk-level baseline metrics.
type BaselineChunkStats struct {
	Total                int64   `json:"total"`
	AvgChunksPerDocument float64 `json:"avg_chunks_per_document"`
	AvgChunkSizeBytes    float64 `json:"avg_chunk_size_bytes"`
}

// BaselineFailureStats aggregates extraction failure metrics.
type BaselineFailureStats struct {
	Open       int64          `json:"open"`
	DeadLetter int64          `json:"dead_letter"`
	BySource   map[string]int `json:"by_source_type"`
}

// BaselineCollectionStats holds the most recent collection timestamps per source.
type BaselineCollectionStats struct {
	MostRecentCollectedAt *time.Time            `json:"most_recent_collected_at"`
	BySource              map[string]*time.Time `json:"by_source_type"`
}

// BaselineStats is the top-level structure returned by the baseline stats query.
type BaselineStats struct {
	Documents          BaselineDocumentStats   `json:"documents"`
	Chunks             BaselineChunkStats      `json:"chunks"`
	ExtractionFailures BaselineFailureStats    `json:"extraction_failures"`
	Collection         BaselineCollectionStats `json:"collection"`
}

// QueryBaselineStats executes four independent queries and assembles BaselineStats.
// Each query is kept separate for readability and to avoid a single monstrous CTE.
func (s *DocumentStore) QueryBaselineStats(ctx context.Context) (*BaselineStats, error) {
	docStats, err := s.queryDocumentStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("baseline stats documents: %w", err)
	}

	chunkStats, err := s.queryChunkStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("baseline stats chunks: %w", err)
	}

	failureStats, err := s.queryFailureStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("baseline stats failures: %w", err)
	}

	collectionStats, err := s.queryCollectionStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("baseline stats collection: %w", err)
	}

	return &BaselineStats{
		Documents:          docStats,
		Chunks:             chunkStats,
		ExtractionFailures: failureStats,
		Collection:         collectionStats,
	}, nil
}

// queryDocumentStats returns per-source document counts and content-length percentiles.
func (s *DocumentStore) queryDocumentStats(ctx context.Context) (BaselineDocumentStats, error) {
	const q = `
		SELECT
			source_type,
			COUNT(*)::bigint                                                        AS cnt,
			AVG(LENGTH(content))::double precision                                  AS mean_len,
			PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY LENGTH(content))::double precision AS p50_len,
			PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY LENGTH(content))::double precision AS p95_len,
			MAX(LENGTH(content))::bigint                                            AS max_len
		FROM documents
		WHERE status = 'active'
		GROUP BY source_type`

	rows, err := s.pg.pool.Query(ctx, q)
	if err != nil {
		return BaselineDocumentStats{}, fmt.Errorf("document stats query: %w", err)
	}
	defer rows.Close()

	bySource := make(map[string]DocumentSourceStats)
	total := 0
	for rows.Next() {
		var (
			src                     string
			cnt                     int64
			meanLen, p50Len, p95Len float64
			maxLen                  int64
		)
		if err := rows.Scan(&src, &cnt, &meanLen, &p50Len, &p95Len, &maxLen); err != nil {
			return BaselineDocumentStats{}, fmt.Errorf("document stats scan: %w", err)
		}
		bySource[src] = DocumentSourceStats{
			Count: int(cnt),
			ContentLength: ContentLengthStats{
				Mean: meanLen,
				P50:  p50Len,
				P95:  p95Len,
				Max:  maxLen,
			},
		}
		total += int(cnt)
	}
	if err := rows.Err(); err != nil {
		return BaselineDocumentStats{}, fmt.Errorf("document stats iter: %w", err)
	}

	return BaselineDocumentStats{Total: total, BySource: bySource}, nil
}

// queryChunkStats returns aggregate chunk metrics across all documents.
func (s *DocumentStore) queryChunkStats(ctx context.Context) (BaselineChunkStats, error) {
	const q = `
		SELECT
			COUNT(*)::bigint                               AS total_chunks,
			COALESCE(AVG(byte_size), 0)::double precision AS avg_chunk_size,
			COALESCE(
				COUNT(*)::double precision / NULLIF(COUNT(DISTINCT document_id), 0),
				0
			)                                              AS avg_per_doc
		FROM chunks`

	var stats BaselineChunkStats
	if err := s.pg.pool.QueryRow(ctx, q).Scan(
		&stats.Total,
		&stats.AvgChunkSizeBytes,
		&stats.AvgChunksPerDocument,
	); err != nil {
		return BaselineChunkStats{}, fmt.Errorf("chunk stats query: %w", err)
	}
	return stats, nil
}

// queryFailureStats returns open and dead-letter extraction failure counts per source.
func (s *DocumentStore) queryFailureStats(ctx context.Context) (BaselineFailureStats, error) {
	const q = `
		SELECT
			source_type,
			COUNT(*) FILTER (WHERE NOT dead_letter)::bigint AS open_cnt,
			COUNT(*) FILTER (WHERE dead_letter)::bigint     AS dead_cnt
		FROM extraction_failures
		GROUP BY source_type`

	rows, err := s.pg.pool.Query(ctx, q)
	if err != nil {
		return BaselineFailureStats{}, fmt.Errorf("failure stats query: %w", err)
	}
	defer rows.Close()

	bySource := make(map[string]int)
	var totalOpen, totalDead int64
	for rows.Next() {
		var (
			src              string
			openCnt, deadCnt int64
		)
		if err := rows.Scan(&src, &openCnt, &deadCnt); err != nil {
			return BaselineFailureStats{}, fmt.Errorf("failure stats scan: %w", err)
		}
		bySource[src] = int(openCnt + deadCnt)
		totalOpen += openCnt
		totalDead += deadCnt
	}
	if err := rows.Err(); err != nil {
		return BaselineFailureStats{}, fmt.Errorf("failure stats iter: %w", err)
	}

	return BaselineFailureStats{
		Open:       totalOpen,
		DeadLetter: totalDead,
		BySource:   bySource,
	}, nil
}

// queryCollectionStats returns the most recent collected_at per source type.
func (s *DocumentStore) queryCollectionStats(ctx context.Context) (BaselineCollectionStats, error) {
	const q = `
		SELECT source_type, MAX(collected_at)
		FROM documents
		WHERE status = 'active'
		GROUP BY source_type`

	rows, err := s.pg.pool.Query(ctx, q)
	if err != nil {
		return BaselineCollectionStats{}, fmt.Errorf("collection stats query: %w", err)
	}
	defer rows.Close()

	bySource := make(map[string]*time.Time)
	var mostRecent *time.Time
	for rows.Next() {
		var (
			src string
			ts  time.Time
		)
		if err := rows.Scan(&src, &ts); err != nil {
			return BaselineCollectionStats{}, fmt.Errorf("collection stats scan: %w", err)
		}
		t := ts // local copy for pointer
		bySource[src] = &t
		if mostRecent == nil || ts.After(*mostRecent) {
			mostRecent = &t
		}
	}
	if err := rows.Err(); err != nil {
		return BaselineCollectionStats{}, fmt.Errorf("collection stats iter: %w", err)
	}

	return BaselineCollectionStats{
		MostRecentCollectedAt: mostRecent,
		BySource:              bySource,
	}, nil
}

// ListUnembedded returns up to limit active documents whose embedding column is
// NULL, ordered by collected_at ASC (oldest first) so backfill progresses
// forward in time.
//
// Soft-deleted documents are excluded because they are never served in search
// results and re-embedding them would waste API quota.
func (s *DocumentStore) ListUnembedded(ctx context.Context, limit int) ([]*model.Document, error) {
	const q = `
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding
		FROM documents
		WHERE embedding IS NULL
		  AND status = 'active'
		ORDER BY collected_at ASC
		LIMIT $1`

	rows, err := s.pg.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list unembedded: %w", err)
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// ListDocumentsNeedingEmbedding 은 "임베딩을 (다시) 만들어야 하는" 활성 문서를
// 최대 limit 건 돌려준다. 대상은 두 부류다:
//
//  1. embedding IS NULL — 아직 임베딩되지 않은 문서(ListUnembedded 와 동일).
//  2. embedding_version 이 currentVersion 과 다른 문서 — 다른 모델/차원/입력
//     구성으로 만들어진 옛 벡터. NULL(마이그레이션 037 이전 레거시)도 포함된다.
//
// currentVersion 이 빈 문자열이면 2번 조건을 적용하지 않는다(= ListUnembedded
// 와 같은 동작). 임베딩 버전이 배선되지 않았거나 재임베딩이 꺼져 있을 때
// 기존 동작을 한 치도 바꾸지 않기 위한 안전 기본값이다.
//
// 정렬은 collected_at ASC — 오래된 문서부터 앞으로 진행한다.
func (s *DocumentStore) ListDocumentsNeedingEmbedding(ctx context.Context, limit int, currentVersion string) ([]*model.Document, error) {
	if currentVersion == "" {
		return s.ListUnembedded(ctx, limit)
	}

	const q = `
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding
		FROM documents
		WHERE status = 'active'
		  AND (embedding IS NULL OR embedding_version IS DISTINCT FROM $2)
		ORDER BY collected_at ASC
		LIMIT $1`

	rows, err := s.pg.pool.Query(ctx, q, limit, currentVersion)
	if err != nil {
		return nil, fmt.Errorf("list documents needing embedding: %w", err)
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// CountDocumentsNeedingEmbedding 은 재임베딩 대상 활성 문서 수를 센다.
// 진행 상황 로그용이며, 검색 경로에서는 호출하지 않는다.
func (s *DocumentStore) CountDocumentsNeedingEmbedding(ctx context.Context, currentVersion string) (int, error) {
	var n int
	err := s.pg.pool.QueryRow(ctx, `
		SELECT count(*) FROM documents
		WHERE status = 'active'
		  AND (embedding IS NULL OR ($1 <> '' AND embedding_version IS DISTINCT FROM $1))`,
		currentVersion,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count documents needing embedding: %w", err)
	}
	return n, nil
}

// listWithoutEntitiesQuery backs ListWithoutEntities.
//
// The source_type filter is load-bearing, not cosmetic: without it the
// EntityWorker picks up every model-derived insight document and spends an LLM
// call extracting entities from an inference. Those entity links then feed the
// entity RRF lane in hybridSearch, which is how an unlabelled inference earns
// its way back into retrieval — the exact loop the insight-exclusion guard in
// search.Service exists to close. Exclusion is by source_type rather than by
// metadata so it cannot be undone by an enrichment-status edit.
const listWithoutEntitiesQuery = `
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding
		FROM documents
		WHERE status = 'active'
		  AND entities_processed_at IS NULL
		  AND source_type <> 'insight'
		ORDER BY collected_at ASC
		LIMIT $1`

// ListWithoutEntities returns up to limit active documents whose
// entities_processed_at column is NULL, ordered by collected_at ASC (oldest
// first) so that entity-extraction backfill progresses forward in time.
//
// Once the EntityWorker attempts extraction for a document — whether or not
// any entities are found — it calls MarkEntitiesProcessed to set the column,
// preventing the document from being re-queued on subsequent ticks.
//
// Soft-deleted documents are excluded — there is no value in extracting
// entities from documents that are not served in search results.
//
// Insight documents are excluded too; see listWithoutEntitiesQuery.
func (s *DocumentStore) ListWithoutEntities(ctx context.Context, limit int) ([]*model.Document, error) {
	rows, err := s.pg.pool.Query(ctx, listWithoutEntitiesQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("list without entities: %w", err)
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// MarkEntitiesProcessed sets entities_processed_at to now() for the given
// document. After this call the document will no longer be returned by
// ListWithoutEntities, preventing the EntityWorker from re-queuing it on
// every tick when entity extraction consistently returns zero results.
func (s *DocumentStore) MarkEntitiesProcessed(ctx context.Context, documentID uuid.UUID) error {
	const q = `UPDATE documents SET entities_processed_at = now() WHERE id = $1 AND entities_processed_at IS NULL`
	if _, err := s.pg.pool.Exec(ctx, q, documentID); err != nil {
		return fmt.Errorf("mark entities processed %s: %w", documentID, err)
	}
	return nil
}

// ListPendingForExtraction returns up to limit active documents from the 4
// active sources (gmail, sms, call-log, call-transcript) whose occurred_at
// falls within the last 30 days AND whose relations_extracted_at is NULL
// AND whose extraction backoff (if any) has elapsed (spec §5.1, migration
// 023). The 30-day bound is evaluated live at every call — NOT a one-time
// snapshot — so newly collected documents (which always have recent
// occurred_at) are naturally picked up on later ticks without any extra
// state, satisfying spec §5.1's "이후에는 신규 유입 문서를 증분 처리한다".
//
// Deliberately keyed on relations_extracted_at, NOT entities_processed_at:
// that column belongs to the pre-existing, currently-active EntityWorker
// (ENTITY_EXTRACTION_ENABLED=true in production), which extracts a
// different artifact via a different LLM call. See migration 023's header
// comment for the full rationale.
func (s *DocumentStore) ListPendingForExtraction(ctx context.Context, limit int) ([]*model.Document, error) {
	const q = `
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding
		FROM documents
		WHERE status = 'active'
		  AND relations_extracted_at IS NULL
		  AND source_type IN ('gmail', 'sms', 'call-log', 'call-transcript')
		  AND occurred_at >= now() - interval '30 days'
		  AND (metadata->>'extraction_next_retry_at' IS NULL
		       OR (metadata->>'extraction_next_retry_at')::timestamptz <= now())
		ORDER BY occurred_at ASC
		LIMIT $1`

	rows, err := s.pg.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending for extraction: %w", err)
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// MarkRelationsExtracted sets relations_extracted_at to now() on successful
// extraction. After this call the document is never returned by
// ListPendingForExtraction again (migration 023's partial index scopes on
// this column being NULL).
func (s *DocumentStore) MarkRelationsExtracted(ctx context.Context, documentID uuid.UUID) error {
	const q = `UPDATE documents SET relations_extracted_at = now() WHERE id = $1 AND relations_extracted_at IS NULL`
	if _, err := s.pg.pool.Exec(ctx, q, documentID); err != nil {
		return fmt.Errorf("mark relations extracted %s: %w", documentID, err)
	}
	return nil
}

// MarkExtractionAttemptFailed records a non-terminal extraction failure
// (attempts 1-2 of a 3-attempt cap — mirrors NoteEnrichmentWorker's retry
// model, adopted deliberately over EntityWorker's unbounded-retry model;
// see plan Global Constraints). relations_extracted_at is left NULL so the
// document is retried once nextRetryAt elapses.
func (s *DocumentStore) MarkExtractionAttemptFailed(ctx context.Context, documentID uuid.UUID, attempts int, reason string, nextRetryAt time.Time) error {
	const q = `
		UPDATE documents
		SET metadata = metadata || jsonb_build_object(
		        'extraction_attempts', $2::int,
		        'extraction_last_error', $3::text,
		        'extraction_next_retry_at', $4::timestamptz
		    ),
		    updated_at = now()
		WHERE id = $1`
	if _, err := s.pg.pool.Exec(ctx, q, documentID, attempts, reason, nextRetryAt); err != nil {
		return fmt.Errorf("mark extraction attempt failed %s: %w", documentID, err)
	}
	return nil
}

// MarkExtractionTerminal records the 3rd (terminal) extraction failure:
// relations_extracted_at is set so ListPendingForExtraction stops returning
// the document (a permanently malformed LLM output must not retry forever
// — this is the exact failure mode this plan's JSON-decoder defense and
// retry cap both exist to bound). There is currently no manual-retry
// endpoint for extraction (unlike POST /api/v1/notes/{id}/retry-enrichment)
// — out of scope for Part A; add one in Part C if operational need arises.
func (s *DocumentStore) MarkExtractionTerminal(ctx context.Context, documentID uuid.UUID, reason string) error {
	const q = `
		UPDATE documents
		SET relations_extracted_at = now(),
		    metadata = metadata || jsonb_build_object('extraction_last_error', $2::text),
		    updated_at = now()
		WHERE id = $1`
	if _, err := s.pg.pool.Exec(ctx, q, documentID, reason); err != nil {
		return fmt.Errorf("mark extraction terminal %s: %w", documentID, err)
	}
	return nil
}

// ListPendingNotes returns up to limit active model.SourceNote documents
// whose Metadata.enrichment_status is "pending" and whose
// enrichment_next_retry_at backoff (if any) has elapsed. Used by
// NoteEnrichmentWorker (spec §6.3). insight documents are never returned —
// source_type is hard-scoped to 'note', enforcing spec §3.2 gate 1
// (insight-of-insight is structurally impossible via this query).
func (s *DocumentStore) ListPendingNotes(ctx context.Context, limit int) ([]*model.Document, error) {
	const q = `
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding
		FROM documents
		WHERE source_type = 'note'
		  AND status = 'active'
		  AND metadata->>'enrichment_status' = 'pending'
		  AND (metadata->>'enrichment_next_retry_at' IS NULL
		       OR (metadata->>'enrichment_next_retry_at')::timestamptz <= now())
		ORDER BY collected_at ASC
		LIMIT $1`

	rows, err := s.pg.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending notes: %w", err)
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// MarkNoteEnriched merges the Organized-layer fields (spec §3) into a note's
// Metadata and marks enrichment_status "done". title, when non-empty,
// overwrites the document's Title column — this is how POST /api/v1/notes'
// server-left-empty title (spec §6.1) gets filled in. Content is never
// touched by this method or any caller of it (spec §3.1 write-once
// guarantee) — there is no content parameter to accidentally pass one.
func (s *DocumentStore) MarkNoteEnriched(ctx context.Context, documentID uuid.UUID, title string, metadata map[string]any) error {
	meta, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("mark note enriched: marshal metadata: %w", err)
	}
	const q = `
		UPDATE documents
		SET title = CASE WHEN $2 <> '' THEN $2 ELSE title END,
		    metadata = metadata || $3::jsonb,
		    updated_at = now()
		WHERE id = $1`
	if _, err := s.pg.pool.Exec(ctx, q, documentID, title, meta); err != nil {
		return fmt.Errorf("mark note enriched %s: %w", documentID, err)
	}
	return nil
}

// MarkNoteEnrichmentAttemptFailed records a non-terminal enrichment failure
// (attempts 1-2 of the 3-attempt cap, spec §6.3): increments the attempt
// counter, records the failure reason, and schedules the next eligible
// retry time. enrichment_status is left "pending" so ListPendingNotes will
// pick the note back up once nextRetryAt elapses.
func (s *DocumentStore) MarkNoteEnrichmentAttemptFailed(ctx context.Context, documentID uuid.UUID, attempts int, reason string, nextRetryAt time.Time) error {
	const q = `
		UPDATE documents
		SET metadata = metadata || jsonb_build_object(
		        'enrichment_attempts', $2::int,
		        'enrichment_last_error', $3::text,
		        'enrichment_next_retry_at', $4::timestamptz
		    ),
		    updated_at = now()
		WHERE id = $1`
	if _, err := s.pg.pool.Exec(ctx, q, documentID, attempts, reason, nextRetryAt); err != nil {
		return fmt.Errorf("mark note enrichment attempt failed %s: %w", documentID, err)
	}
	return nil
}

// MarkNoteEnrichmentTerminal records the 3rd (terminal) enrichment failure
// (spec §6.3): sets enrichment_status "failed" so ListPendingNotes stops
// returning the note. Only POST /api/v1/notes/{id}/retry-enrichment
// (ResetNoteEnrichment) can revive it.
func (s *DocumentStore) MarkNoteEnrichmentTerminal(ctx context.Context, documentID uuid.UUID, reason string) error {
	const q = `
		UPDATE documents
		SET metadata = metadata || jsonb_build_object(
		        'enrichment_status', 'failed',
		        'enrichment_last_error', $2::text
		    ),
		    updated_at = now()
		WHERE id = $1`
	if _, err := s.pg.pool.Exec(ctx, q, documentID, reason); err != nil {
		return fmt.Errorf("mark note enrichment terminal %s: %w", documentID, err)
	}
	return nil
}

// ResetNoteEnrichment reverts a terminal "failed" note back to "pending"
// with a zeroed attempt counter, for manual retry via
// POST /api/v1/notes/{id}/retry-enrichment (spec §6.3, §9.2). Returns
// found=false (no error) when documentID does not exist, is not a note, or
// is not currently in the terminal "failed" state — the caller (Task 7
// retryEnrichmentHandler) maps found=false to 409 Conflict.
func (s *DocumentStore) ResetNoteEnrichment(ctx context.Context, documentID uuid.UUID) (bool, error) {
	const q = `
		UPDATE documents
		SET metadata = metadata || jsonb_build_object(
		        'enrichment_status', 'pending',
		        'enrichment_attempts', 0,
		        'enrichment_last_error', NULL,
		        'enrichment_next_retry_at', NULL
		    ),
		    updated_at = now()
		WHERE id = $1
		  AND source_type = 'note'
		  AND metadata->>'enrichment_status' = 'failed'
		RETURNING id`
	var returnedID uuid.UUID
	err := s.pg.pool.QueryRow(ctx, q, documentID).Scan(&returnedID)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("reset note enrichment %s: %w", documentID, err)
	}
	return true, nil
}

// SoftDeleteByID soft-deletes a single model.SourceNote document (spec §6.5
// delete path). Scoped to source_type='note' so this method cannot be used
// to soft-delete arbitrary documents of other source types. Returns nil
// without effect when the id does not exist or the document is not an
// active note — callers that need to distinguish "not found" from
// "not a note" should GetByID first (Task 7 deleteNoteHandler does this).
func (s *DocumentStore) SoftDeleteByID(ctx context.Context, id uuid.UUID) error {
	const q = `
		UPDATE documents
		SET status = 'deleted', deleted_at = now()
		WHERE id = $1 AND source_type = 'note' AND status = 'active'`
	if _, err := s.pg.pool.Exec(ctx, q, id); err != nil {
		return fmt.Errorf("soft delete note %s: %w", id, err)
	}
	return nil
}

// SoftDeleteInsightsByNoteID cascades a note's soft-delete to every
// model.SourceInsight document derived from it (spec §6.5 permanent
// policy): "근거가 사라진 추론은 반증 불가능하고 감사 불가능하다". Matches
// on the JSONB path Metadata.provenance.source_note_id, which is how
// insight documents record their origin note (Task 6 EnrichNote). Returns
// the number of insight documents soft-deleted.
func (s *DocumentStore) SoftDeleteInsightsByNoteID(ctx context.Context, noteID uuid.UUID) (int, error) {
	const q = `
		UPDATE documents
		SET status = 'deleted', deleted_at = now()
		WHERE source_type = 'insight'
		  AND status = 'active'
		  AND metadata->'provenance'->>'source_note_id' = $1
		RETURNING id`
	rows, err := s.pg.pool.Query(ctx, q, noteID.String())
	if err != nil {
		return 0, fmt.Errorf("soft delete insights for note %s: %w", noteID, err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("soft delete insights for note %s: iterate: %w", noteID, err)
	}
	return count, nil
}

// listUnsummarizedQuery backs ListUnsummarized.
//
// The source_type filter is load-bearing, not cosmetic: without it the
// SummarizerWorker picks up every model-derived insight document and pays for
// an LLM summary of an LLM inference — a document that is already a single
// sentence. That is unbudgeted spend per note, on output nobody reads.
const listUnsummarizedQuery = `
		SELECT id, source_type, source_id, title, content, metadata, embedding,
		       status, deleted_at, occurred_at, collected_at, created_at, updated_at,
		       title_summary, bullet_summary, summary_embedding
		FROM documents
		WHERE title_summary IS NULL
		  AND status = 'active'
		  AND source_type <> 'insight'
		ORDER BY collected_at ASC
		LIMIT $1`

// ListUnsummarized returns up to limit active documents whose title_summary
// column is NULL, ordered by collected_at ASC (oldest first) so backfill
// progresses forward in time.
//
// Soft-deleted documents are excluded; there is no value in summarizing them.
// Insight documents are excluded too; see listUnsummarizedQuery.
func (s *DocumentStore) ListUnsummarized(ctx context.Context, limit int) ([]*model.Document, error) {
	rows, err := s.pg.pool.Query(ctx, listUnsummarizedQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("list unsummarized: %w", err)
	}
	defer rows.Close()

	return collectDocuments(rows)
}

// SummaryCoverageRatio returns the fraction of active documents that have a
// summary_embedding (non-NULL).  The result is cached for summaryCoverageTTL
// (60 s) to avoid a per-search COUNT(*) on large tables.
//
// Used by hybridSearch to gate the SummaryVec signal (#63): when coverage is
// below model.SummaryVecCoverageThreshold() the weight is set to 0 so that
// un-summarised documents are not systematically demoted during backfill.
func (s *DocumentStore) SummaryCoverageRatio(ctx context.Context) (float64, error) {
	s.coverageMu.Lock()
	defer s.coverageMu.Unlock()

	if time.Since(s.coverageFetchedAt) < summaryCoverageTTL {
		return s.coverageRatio, nil
	}

	var ratio float64
	err := s.pg.pool.QueryRow(ctx, `
		SELECT COALESCE(
			COUNT(*) FILTER (WHERE summary_embedding IS NOT NULL)::float
			/ NULLIF(COUNT(*), 0),
		0)
		FROM documents
		WHERE status = 'active'`).Scan(&ratio)
	if err != nil {
		return 0, fmt.Errorf("summary coverage ratio: %w", err)
	}

	s.coverageRatio = ratio
	s.coverageFetchedAt = time.Now()
	return ratio, nil
}

// UpdateSummary writes the LLM-generated summary fields for a single document
// identified by its primary key. Only title_summary, bullet_summary, and
// summary_embedding are touched; other fields remain unchanged.
//
// summaryEmbedding may be nil when the embedder is disabled or failed — in
// that case the column is set to NULL, leaving the document out of
// summary-vector search until a subsequent run embeds it.
//
// Idempotency guard: the UPDATE is a no-op when title_summary is already set
// (WHERE title_summary IS NULL).  This prevents a racing concurrent instance
// from overwriting a completed summary with its own version (#64).
func (s *DocumentStore) UpdateSummary(ctx context.Context, id uuid.UUID, titleSummary, bulletSummary string, summaryEmbedding []float32) error {
	var vecArg interface{}
	if len(summaryEmbedding) > 0 {
		vecArg = pgvector.NewVector(summaryEmbedding)
	}
	_, err := s.pg.pool.Exec(ctx, `
		UPDATE documents
		SET title_summary     = $1,
		    bullet_summary    = $2,
		    summary_embedding = $3,
		    updated_at        = now()
		WHERE id = $4
		  AND title_summary IS NULL`,
		titleSummary,
		bulletSummary,
		vecArg,
		id,
	)
	if err != nil {
		return fmt.Errorf("update summary %s: %w", id, err)
	}
	return nil
}

// UpdateEmbedding writes the given embedding vector for a single document
// identified by its primary key. Only the embedding column is touched so that
// other fields (title, content, collected_at …) remain unchanged.
func (s *DocumentStore) UpdateEmbedding(ctx context.Context, doc *model.Document) error {
	if len(doc.Embedding) == 0 {
		return fmt.Errorf("UpdateEmbedding: empty embedding for document %s", doc.ID)
	}
	// embedding_version 은 $3 이 빈 문자열일 때만 기존 값을 유지한다.
	// 버전을 남기지 않으면 재임베딩 선별 쿼리가 같은 행을 영원히 다시 집어
	// 가므로(무한 재임베딩), 벡터와 버전은 반드시 같은 UPDATE 에서 쓴다.
	_, err := s.pg.pool.Exec(ctx, `
		UPDATE documents
		SET embedding = $1,
		    embedding_version = CASE WHEN $3 = '' THEN embedding_version ELSE $3 END,
		    updated_at = now()
		WHERE id = $2`,
		pgvector.NewVector(doc.Embedding),
		doc.ID,
		doc.EmbeddingVersion,
	)
	if err != nil {
		return fmt.Errorf("update embedding %s: %w", doc.ID, err)
	}
	return nil
}

// ListActiveSourceIDs returns all source_ids for active documents of a given source type.
func (s *DocumentStore) ListActiveSourceIDs(ctx context.Context, sourceType model.SourceType) ([]string, error) {
	rows, err := s.pg.pool.Query(ctx, `
		SELECT source_id FROM documents
		WHERE source_type = $1 AND status = 'active'`,
		sourceType,
	)
	if err != nil {
		return nil, fmt.Errorf("list active source IDs for %s: %w", sourceType, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ActiveSourceIDSet returns a set of all source_ids that are currently active
// for the given source type. The returned map is keyed by source_id and is
// safe to use for O(1) membership tests. It is used by the filesystem collector
// to detect files that are new (not yet indexed) regardless of their mtime.
func (s *DocumentStore) ActiveSourceIDSet(ctx context.Context, sourceType model.SourceType) (map[string]struct{}, error) {
	rows, err := s.pg.pool.Query(ctx, `
		SELECT source_id FROM documents
		WHERE source_type = $1 AND status = 'active'`,
		sourceType,
	)
	if err != nil {
		return nil, fmt.Errorf("active source ID set for %s: %w", sourceType, err)
	}
	defer rows.Close()

	set := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		set[id] = struct{}{}
	}
	return set, rows.Err()
}

// --- scan helpers ---

type scannable interface {
	Scan(dest ...interface{}) error
}

func scanDocument(row scannable) (*model.Document, error) {
	var (
		doc       model.Document
		metaJSON  []byte
		vec       *pgvector.Vector
		titleSum  pgtype.Text
		bulletSum pgtype.Text
		summVec   *pgvector.Vector
	)
	err := row.Scan(
		&doc.ID, &doc.SourceType, &doc.SourceID,
		&doc.Title, &doc.Content, &metaJSON, &vec,
		&doc.Status, &doc.DeletedAt,
		&doc.OccurredAt, &doc.CollectedAt, &doc.CreatedAt, &doc.UpdatedAt,
		&titleSum, &bulletSum, &summVec,
	)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(metaJSON, &doc.Metadata); err != nil {
		doc.Metadata = map[string]any{}
	}
	if vec != nil {
		doc.Embedding = vec.Slice()
	}
	// pgtype.Text: NULL columns arrive as Valid=false; avoid assigning zero string
	// from a non-pointer Scan which pgx v5 rejects for nullable text columns.
	if titleSum.Valid {
		doc.TitleSummary = titleSum.String
	}
	if bulletSum.Valid {
		doc.BulletSummary = bulletSum.String
	}
	if summVec != nil {
		doc.SummaryEmbedding = summVec.Slice()
	}
	return &doc, nil
}

func collectDocuments(rows pgx.Rows) ([]*model.Document, error) {
	var docs []*model.Document
	for rows.Next() {
		doc, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

func collectResults(rows pgx.Rows, matchType string) ([]*model.SearchResult, error) {
	var results []*model.SearchResult
	for rows.Next() {
		var (
			r         model.SearchResult
			metaJSON  []byte
			vec       *pgvector.Vector
			titleSum  pgtype.Text
			bulletSum pgtype.Text
			summVec   *pgvector.Vector
		)
		err := rows.Scan(
			&r.ID, &r.SourceType, &r.SourceID,
			&r.Title, &r.Content, &metaJSON, &vec,
			&r.Status, &r.DeletedAt,
			&r.OccurredAt, &r.CollectedAt, &r.CreatedAt, &r.UpdatedAt,
			&titleSum, &bulletSum, &summVec,
			&r.Score,
		)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(metaJSON, &r.Metadata); err != nil {
			r.Metadata = map[string]any{}
		}
		if vec != nil {
			r.Embedding = vec.Slice()
		}
		if titleSum.Valid {
			r.TitleSummary = titleSum.String
		}
		if bulletSum.Valid {
			r.BulletSummary = bulletSum.String
		}
		if summVec != nil {
			r.SummaryEmbedding = summVec.Slice()
		}
		r.MatchType = matchType
		results = append(results, &r)
	}
	return results, rows.Err()
}
