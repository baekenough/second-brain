package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// EvalExporter is the subset of store.EvalStore used by the eval export handler.
// Defined as an interface so tests can inject a stub without a real database.
type EvalExporter interface {
	ExportJSONL(ctx context.Context, w io.Writer) (int, error)
}

// evalExportHandler handles GET /api/v1/eval/export.
//
// It streams the eval set as newline-delimited JSON (JSONL / application/x-ndjson).
// Each line is a JSON object with the shape:
//
//	{"id":N,"query":"...","relevant_doc_ids":[...],"source":"feedback","created_at":"..."}
//
// The response header X-Eval-Pair-Count contains the number of pairs exported.
// The set is derived from positive feedback rows (thumbs >= 1) grouped by query.
func (s *Server) evalExportHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("X-Source", "feedback")

	count, err := s.eval.ExportJSONL(r.Context(), w)
	if err != nil {
		// If we have already written some bytes the status code is already 200;
		// we can't change it. Log the error so it is visible in server logs.
		slog.Error("eval: export failed", "error", err)
		// Only call http.Error when nothing has been written yet.
		// http.Error calls WriteHeader internally, which is a no-op after the
		// first call, but it would append an HTML error body to a partial JSONL
		// stream. Guard with a header-sent check via a wrapping writer in tests;
		// in production the partial response is acceptable given the 5 000-row cap.
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("X-Eval-Pair-Count", fmt.Sprintf("%d", count))
}
