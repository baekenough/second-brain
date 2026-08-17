package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/note"
)

// notesRequestEnvelopeMaxBytes bounds the whole JSON request body for
// POST /api/v1/notes: note.MaxContentBytes (10 MiB) + note.MaxTitleBytes
// (1 KiB) + a small margin for JSON structure overhead.
const notesRequestEnvelopeMaxBytes = note.MaxContentBytes + note.MaxTitleBytes + 4096

// NotesUpserter is the document persistence interface required by the notes
// handlers. *store.DocumentStore satisfies this interface (Task 5).
type NotesUpserter interface {
	note.DocumentUpserter
	GetByID(ctx context.Context, id uuid.UUID) (*model.Document, error)
	ResetNoteEnrichment(ctx context.Context, documentID uuid.UUID) (bool, error)
	SoftDeleteByID(ctx context.Context, id uuid.UUID) error
	SoftDeleteInsightsByNoteID(ctx context.Context, noteID uuid.UUID) (int, error)
}

// CreateNoteResponse is the JSON body returned by POST /api/v1/notes.
type CreateNoteResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// createNoteRequest is the JSON body for POST /api/v1/notes.
type createNoteRequest struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}

// WithNotes attaches the dependencies required by POST /api/v1/notes,
// POST /api/v1/notes/{id}/retry-enrichment, and DELETE /api/v1/notes/{id},
// and enables all three routes. Must be called before the first call to
// Handler().
func (s *Server) WithNotes(upserter NotesUpserter, chunks IngestFileChunkWriter, embed IngestFileEmbedder) *Server {
	s.notesUpserter = upserter
	s.notesChunks = chunks
	s.notesEmbedder = embed
	return s
}

// createNoteHandler handles POST /api/v1/notes (spec §6.1). Persists the
// raw note synchronously via note.Save and returns 202 immediately;
// enrichment happens asynchronously via NoteEnrichmentWorker (Task 6).
func (s *Server) createNoteHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, notesRequestEnvelopeMaxBytes)

	var req createNoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if isMaxBytesError(err) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds maximum size of %d bytes", notesRequestEnvelopeMaxBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// note.Save also validates content/title, but the size checks are
	// duplicated here so the correct HTTP status (413 vs 400) can be chosen —
	// note.Save returns a single free-text errMsg with no status hint.
	if len(req.Content) > note.MaxContentBytes {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("content exceeds maximum size of %d bytes", note.MaxContentBytes))
		return
	}
	if len(req.Title) > note.MaxTitleBytes {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("title exceeds maximum size of %d bytes", note.MaxTitleBytes))
		return
	}

	metadata := map[string]any{
		"enrichment_status":   "pending",
		"enrichment_attempts": 0,
	}

	result, errMsg := note.Save(r.Context(), s.notesUpserter, s.notesChunks, s.notesEmbedder,
		model.SourceNote, req.Title, req.Content, "", metadata, true, false /* requireTitle */)
	if errMsg != "" {
		// note.Save's remaining validation failures (empty content after
		// trimming) are 400s; anything else is an internal error string.
		if errMsg == "content is required and must be non-empty" {
			writeError(w, http.StatusBadRequest, errMsg)
			return
		}
		slog.Error("create_note: save failed", "error", errMsg)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusAccepted, CreateNoteResponse{ID: result.ID, Status: "pending"})
}

// retryEnrichmentHandler handles POST /api/v1/notes/{id}/retry-enrichment
// (spec §6.3, §9.2). Only a note whose enrichment_status is the terminal
// value "failed" can be retried; any other state (including "not found")
// returns an error rather than silently reviving a note that is not stuck.
func (s *Server) retryEnrichmentHandler(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid note id")
		return
	}

	doc, err := s.notesUpserter.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "note not found")
		return
	}
	if doc.SourceType != model.SourceNote {
		writeError(w, http.StatusNotFound, "note not found")
		return
	}

	reset, err := s.notesUpserter.ResetNoteEnrichment(r.Context(), id)
	if err != nil {
		slog.Error("retry_enrichment: reset failed", "doc_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if !reset {
		writeError(w, http.StatusConflict, "note is not in a terminal failed state")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"id": id.String(), "status": "pending"})
}

// deleteNoteHandler handles DELETE /api/v1/notes/{id} (spec §6.5 cascade
// delete). Soft-deletes the note and cascades to every insight document
// derived from it. Only source_type=note documents can be deleted through
// this endpoint. A cascade failure is logged but does not fail the request —
// the note itself is already deleted; a stray orphaned insight is a
// lesser failure mode than a partially-applied delete.
func (s *Server) deleteNoteHandler(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid note id")
		return
	}

	doc, err := s.notesUpserter.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "note not found")
		return
	}
	if doc.SourceType != model.SourceNote {
		writeError(w, http.StatusNotFound, "note not found")
		return
	}
	if doc.Status != "active" {
		writeError(w, http.StatusConflict, "note is already deleted")
		return
	}

	if err := s.notesUpserter.SoftDeleteByID(r.Context(), id); err != nil {
		slog.Error("delete_note: soft delete failed", "doc_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	insightsDeleted, err := s.notesUpserter.SoftDeleteInsightsByNoteID(r.Context(), id)
	if err != nil {
		slog.Warn("delete_note: insight cascade failed (note already deleted)", "doc_id", id, "error", err)
		insightsDeleted = 0
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":               id.String(),
		"status":           "deleted",
		"insights_deleted": insightsDeleted,
	})
}
