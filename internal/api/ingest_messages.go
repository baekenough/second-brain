package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/baekenough/second-brain/internal/chunker"
	"github.com/baekenough/second-brain/internal/collector/smsmap"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// defaultIngestMaxBatchMessages is the per-request record count cap used when
// WithIngestMessages receives a zero/negative maxBatchMessages argument.
const defaultIngestMaxBatchMessages = 5000

// IngestMessagesUpserter is the document persistence interface required by the
// ingest-messages handler. *store.DocumentStore satisfies this interface.
//
// Note: this is intentionally identical to IngestFileUpserter so a single
// concrete store implementation can satisfy both without wrapping.
type IngestMessagesUpserter interface {
	Upsert(ctx context.Context, doc *model.Document) error
}

// WithIngestMessages attaches the dependencies required by
// POST /api/v1/ingest/messages and enables the route.
//
// maxBatchMessages caps the total number of SMS + call records accepted in a
// single request; 0 uses the package default (5000). Pass
// cfg.IngestMaxBatchMessages here.
//
// cutover is the optional floor time inherited from cfg.CollectorCutover:
// records whose OccurredAt is before this time are silently skipped.
// Zero time.Time{} disables the floor.
//
// Must be called before the first call to Handler().
func (s *Server) WithIngestMessages(
	upserter IngestMessagesUpserter,
	chunks IngestFileChunkWriter,
	embed IngestFileEmbedder,
	maxBatchMessages int,
	cutover time.Time,
) *Server {
	s.messagesUpserter = upserter
	s.messagesChunks = chunks
	s.messagesEmbedder = embed
	if maxBatchMessages <= 0 {
		s.messagesMaxBatch = defaultIngestMaxBatchMessages
	} else {
		s.messagesMaxBatch = maxBatchMessages
	}
	s.messagesCutover = cutover
	return s
}

// ingestSMSRecord is one element in the "sms" array of the request body.
type ingestSMSRecord struct {
	Address     string `json:"address"`
	Body        string `json:"body"`
	DateMs      int64  `json:"date_ms"`
	Type        int    `json:"type"`
	ContactName string `json:"contact_name"`
}

// ingestCallRecord is one element in the "calls" array of the request body.
type ingestCallRecord struct {
	Number      string `json:"number"`
	DateMs      int64  `json:"date_ms"`
	DurationSec int    `json:"duration_sec"`
	Type        int    `json:"type"`
	ContactName string `json:"contact_name"`
}

// IngestMessagesRequest is the JSON body accepted by
// POST /api/v1/ingest/messages.
type IngestMessagesRequest struct {
	SMS   []ingestSMSRecord  `json:"sms"`
	Calls []ingestCallRecord `json:"calls"`
}

// IngestMessagesResponse is the JSON body returned on a successful request.
type IngestMessagesResponse struct {
	Accepted int      `json:"accepted"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors"`
}

// ingestMessagesHandler handles POST /api/v1/ingest/messages.
//
// Accepts a JSON body with optional "sms" and "calls" arrays. Each record is
// mapped to a model.Document via smsmap.MapSMS / smsmap.MapCall (identical
// semantics to the XML-based SMSCollector), then:
//
//  1. Cutover floor check: records with OccurredAt before messagesCutover are
//     silently skipped (counted in "skipped"). Zero cutover = no floor.
//  2. Idempotent upsert via IngestMessagesUpserter.
//  3. Chunk + embed (mirrors ingest_file.go / add_note; non-fatal on embed failure).
//
// Returns {"accepted": N, "skipped": M, "errors": []} on success (201 Created)
// or an appropriate error status code.
func (s *Server) ingestMessagesHandler(w http.ResponseWriter, r *http.Request) {
	if s.messagesUpserter == nil {
		writeError(w, http.StatusServiceUnavailable, "message ingest not configured")
		return
	}

	var req IngestMessagesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	total := len(req.SMS) + len(req.Calls)
	if total > s.messagesMaxBatch {
		writeError(w, http.StatusRequestEntityTooLarge,
			"batch size exceeds maximum allowed records")
		return
	}

	var (
		accepted int
		skipped  int
		errs     []string
	)

	// Process SMS records.
	for i := range req.SMS {
		rec := &req.SMS[i]
		if rec.Address == "" {
			errs = append(errs, "sms record missing address")
			continue
		}
		if rec.DateMs == 0 {
			errs = append(errs, "sms record missing date_ms")
			continue
		}

		doc := smsmap.MapSMS(rec.Address, rec.Body, rec.DateMs, rec.Type, rec.ContactName)

		// Cutover floor: skip records that pre-date the cutover.
		if !s.messagesCutover.IsZero() && doc.OccurredAt != nil && doc.OccurredAt.Before(s.messagesCutover) {
			skipped++
			continue
		}

		if err := s.upsertAndEmbedMessage(r.Context(), &doc); err != nil {
			slog.Error("ingest_messages: upsert failed",
				"source_id", doc.SourceID, "error", err)
			errs = append(errs, "upsert failed for "+doc.SourceID)
			continue
		}
		accepted++
	}

	// Process call records.
	for i := range req.Calls {
		rec := &req.Calls[i]
		if rec.Number == "" {
			errs = append(errs, "call record missing number")
			continue
		}
		if rec.DateMs == 0 {
			errs = append(errs, "call record missing date_ms")
			continue
		}

		doc := smsmap.MapCall(rec.Number, rec.DateMs, rec.DurationSec, rec.Type, rec.ContactName)

		// Cutover floor: skip records that pre-date the cutover.
		if !s.messagesCutover.IsZero() && doc.OccurredAt != nil && doc.OccurredAt.Before(s.messagesCutover) {
			skipped++
			continue
		}

		if err := s.upsertAndEmbedMessage(r.Context(), &doc); err != nil {
			slog.Error("ingest_messages: upsert failed",
				"source_id", doc.SourceID, "error", err)
			errs = append(errs, "upsert failed for "+doc.SourceID)
			continue
		}
		accepted++
	}

	if errs == nil {
		errs = []string{} // return [] not null
	}
	writeJSON(w, http.StatusCreated, IngestMessagesResponse{
		Accepted: accepted,
		Skipped:  skipped,
		Errors:   errs,
	})
}

// upsertAndEmbedMessage persists doc and triggers chunking + embedding.
// It mirrors the logic in ingestFileHandler / add_note.
// Embedding failures are non-fatal: the document is stored and FTS-searchable.
func (s *Server) upsertAndEmbedMessage(ctx context.Context, doc *model.Document) error {
	if err := s.messagesUpserter.Upsert(ctx, doc); err != nil {
		return err
	}

	// Chunk + embed (mirrors ingest_file.go inline path).
	if s.messagesChunks == nil {
		return nil
	}

	texts := chunker.Split(doc.Content, chunker.SelectOptions(*doc))
	if len(texts) == 0 {
		return nil
	}

	chunkSlice := make([]store.Chunk, 0, len(texts))
	for i, t := range texts {
		chunkSlice = append(chunkSlice, store.Chunk{
			DocumentID: doc.ID,
			ChunkIndex: i,
			Content:    t,
			ByteSize:   len(t),
		})
	}

	if err := s.messagesChunks.ReplaceDocument(ctx, doc.ID, chunkSlice); err != nil {
		return err
	}

	if s.messagesEmbedder != nil && s.messagesEmbedder.Enabled() {
		if embErr := ingestEmbedChunks(ctx, doc.ID, chunkSlice, s.messagesChunks, s.messagesEmbedder); embErr != nil {
			slog.Warn("ingest_messages: embedding failed (non-fatal)",
				"doc_id", doc.ID, "error", embErr)
		}
	}
	return nil
}

