package api

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/model"
)

const rawMaxBytes = 50 * 1024 * 1024 // 50 MiB

// listDocumentsHandler handles GET /api/v1/documents.
//
// Query parameters:
//   - source         — optional SourceType include filter
//   - exclude_source — comma-separated SourceTypes to exclude (e.g. "slack,github")
//   - limit          — default 20, max 100
//   - offset         — default 0
func (s *Server) listDocumentsHandler(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 20)
	if limit > 100 {
		limit = 100
	}
	offset := queryInt(r, "offset", 0)

	var includeSrc model.SourceType
	if v := r.URL.Query().Get("source"); v != "" {
		includeSrc = model.SourceType(v)
	}

	var excludeSrcs []model.SourceType
	if excludeRaw := r.URL.Query().Get("exclude_source"); excludeRaw != "" {
		for _, s := range strings.Split(excludeRaw, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				excludeSrcs = append(excludeSrcs, model.SourceType(s))
			}
		}
	}

	docs, err := s.docs.ListRecent(r.Context(), includeSrc, excludeSrcs, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"documents": docs})
}

// getDocumentHandler handles GET /api/v1/documents/{id}.
func (s *Server) getDocumentHandler(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid document ID")
		return
	}

	doc, err := s.docs.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}

	writeJSON(w, http.StatusOK, doc)
}

// getDocumentRawHandler handles GET /api/v1/documents/{id}/raw.
//
// Streams the original file bytes for documents whose source_type is
// "filesystem". Source IDs that are absolute paths or that escape the
// configured filesystem root are rejected to prevent path-traversal attacks.
// Symbolic links are refused. Files larger than rawMaxBytes return 413.
func (s *Server) getDocumentRawHandler(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid document ID")
		return
	}

	doc, err := s.docs.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}

	if doc.SourceType != model.SourceFilesystem {
		writeError(w, http.StatusBadRequest, "raw content only available for filesystem source")
		return
	}

	// Refuse absolute source IDs — they are never produced by the collector
	// but could be injected via a compromised database row.
	if filepath.IsAbs(doc.SourceID) {
		writeError(w, http.StatusBadRequest, "invalid source path")
		return
	}

	root := filepath.Clean(s.filesystemPath)
	if root == "" {
		writeError(w, http.StatusInternalServerError, "filesystem source not configured")
		return
	}

	// Resolve the full path and verify it stays inside root.
	absPath := filepath.Clean(filepath.Join(root, doc.SourceID))
	if !strings.HasPrefix(absPath, root+string(filepath.Separator)) && absPath != root {
		writeError(w, http.StatusBadRequest, "invalid source path")
		return
	}

	// Use Lstat so symlinks are never followed.
	fi, err := os.Lstat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "file not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not stat file")
		return
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		writeError(w, http.StatusBadRequest, "symbolic links are not served")
		return
	}
	if !fi.Mode().IsRegular() {
		writeError(w, http.StatusBadRequest, "not a regular file")
		return
	}
	if fi.Size() > rawMaxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file exceeds %d byte limit", rawMaxBytes))
		return
	}

	f, err := os.Open(absPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not open file")
		return
	}
	defer f.Close()

	ct := contentTypeForExt(filepath.Ext(absPath))
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Last-Modified", fi.ModTime().UTC().Format(time.RFC1123))

	if _, err := io.Copy(w, f); err != nil {
		// Headers already sent; log only.
		_ = err
	}
}

// contentTypeForExt maps common file extensions to MIME types.
// Falls back to mime.TypeByExtension then application/octet-stream.
func contentTypeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".json":
		return "application/json"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

func queryInt(r *http.Request, key string, defaultVal int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return defaultVal
	}
	return n
}
