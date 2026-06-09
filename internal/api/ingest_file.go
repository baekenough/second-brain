package api

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/chunker"
	"github.com/baekenough/second-brain/internal/collector/extractor"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// defaultIngestMaxFileBytes is the per-upload size cap used when
// WithIngestFile receives a zero/negative maxFileBytes argument.
const defaultIngestMaxFileBytes = 100 << 20 // 100 MiB

// IngestFileUpserter is the document persistence interface required by the
// ingest-file handler. *store.DocumentStore satisfies this interface.
type IngestFileUpserter interface {
	Upsert(ctx context.Context, doc *model.Document) error
}

// IngestFileChunkWriter is the chunk persistence interface required by the
// ingest-file handler. *store.ChunkStore satisfies this interface.
type IngestFileChunkWriter interface {
	ReplaceDocument(ctx context.Context, documentID uuid.UUID, chunks []store.Chunk) error
	ListByDocument(ctx context.Context, documentID uuid.UUID) ([]store.Chunk, error)
	UpdateChunkEmbeddings(ctx context.Context, embeddings []store.ChunkEmbedding) error
}

// IngestFileEmbedder is the embedding interface required by the ingest-file
// handler. The search.EmbeddingEngine satisfies this interface.
type IngestFileEmbedder interface {
	Enabled() bool
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}

// IngestFileResponse is the JSON body returned on a successful upload.
type IngestFileResponse struct {
	DocumentID string `json:"document_id"`
	Accepted   bool   `json:"accepted"`
}

// WithIngestFile attaches the dependencies required by
// POST /api/v1/ingest/file and enables the route.
//
// maxFileBytes is the per-upload size cap in bytes; 0 uses the package
// default (100 MiB). Pass cfg.IngestMaxFileBytes here.
//
// Must be called before the first call to Handler().
func (s *Server) WithIngestFile(
	upserter IngestFileUpserter,
	chunks IngestFileChunkWriter,
	embed IngestFileEmbedder,
	maxFileBytes int64,
) *Server {
	s.ingestUpserter = upserter
	s.ingestChunks = chunks
	s.ingestEmbedder = embed
	if maxFileBytes <= 0 {
		s.ingestMaxFileBytes = defaultIngestMaxFileBytes
	} else {
		s.ingestMaxFileBytes = maxFileBytes
	}
	return s
}

// ingestFileHandler handles POST /api/v1/ingest/file.
//
// Accepts multipart/form-data with:
//   - file   (required) — the document to ingest
//   - title  (optional) — display title; defaults to the original filename
//   - source (optional) — human-readable origin label (stored in metadata)
//   - tags   (optional) — comma-separated tags (stored in metadata)
//
// Behaviour mirrors the add_note MCP tool: upsert document → split into
// chunks → optionally embed chunks (inline, non-fatal on embed failure).
// Re-uploading the identical file is idempotent: the SourceID is a stable
// hash of (filename + raw bytes).
func (s *Server) ingestFileHandler(w http.ResponseWriter, r *http.Request) {
	if s.ingestUpserter == nil {
		writeError(w, http.StatusServiceUnavailable, "file ingest not configured")
		return
	}

	// Enforce the per-upload size limit at the reader level so that
	// ParseMultipartForm respects it.
	maxBytes := s.ingestMaxFileBytes
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	// ParseMultipartForm buffers up to 32 MiB in memory; the rest spools to
	// temp files. We pass maxBytes so large uploads stay on disk.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		if isMaxBytesError(err) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("file exceeds maximum upload size of %d bytes", maxBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}

	// --- Extract the file part ---
	f, fh, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "field 'file' is required")
		return
	}
	defer f.Close() //nolint:errcheck // best-effort cleanup

	originalFilename := filepath.Base(fh.Filename)
	ext := strings.ToLower(filepath.Ext(originalFilename))
	contentType := fh.Header.Get("Content-Type")

	// Read the entire file into memory; bounded by MaxBytesReader above.
	fileBytes, err := io.ReadAll(f)
	if err != nil {
		if isMaxBytesError(err) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("file exceeds maximum upload size of %d bytes", maxBytes))
			return
		}
		slog.Error("ingest_file: read file bytes", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// --- Extract text from the file ---
	text, err := extractFileText(r.Context(), ext, originalFilename, fileBytes)
	if err != nil {
		if err == errUnsupportedFormat {
			writeError(w, http.StatusUnsupportedMediaType,
				fmt.Sprintf("unsupported file format %q; supported: .pdf, .docx, .xlsx, .pptx, .hwpx, .html, .htm, .txt, .md, .text", ext))
			return
		}
		slog.Error("ingest_file: extract text", "filename", originalFilename, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// --- Parse optional form fields ---
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		title = originalFilename
	}
	sourceLabel := strings.TrimSpace(r.FormValue("source"))

	var tags []string
	if raw := strings.TrimSpace(r.FormValue("tags")); raw != "" {
		for _, t := range strings.Split(raw, ",") {
			if v := strings.TrimSpace(t); v != "" {
				tags = append(tags, v)
			}
		}
	}

	// --- Build stable SourceID: hash(filename + content bytes) ---
	sourceID := uploadSourceID(originalFilename, fileBytes)

	// --- Build metadata ---
	meta := map[string]any{
		"original_filename": originalFilename,
		"content_type":      contentType,
	}
	if sourceLabel != "" {
		meta["source"] = sourceLabel
	}
	if len(tags) > 0 {
		meta["tags"] = tags
	}

	// --- Upsert document (mirrors add_note / collector path) ---
	// CollectedAt set explicitly so the document sorts correctly in
	// ORDER BY collected_at DESC queries (see issue #87 note in add_note).
	doc := &model.Document{
		SourceType:  model.SourceUpload,
		SourceID:    sourceID,
		Title:       title,
		Content:     extractor.SanitizeText(extractor.TruncateUTF8(text, extractor.MaxExtractedBytes)),
		Metadata:    meta,
		Status:      "active",
		CollectedAt: time.Now().UTC(),
	}

	if err := s.ingestUpserter.Upsert(r.Context(), doc); err != nil {
		slog.Error("ingest_file: upsert failed", "source_id", sourceID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// --- Chunk + embed (inline, mirrors add_note in cmd/mcp/main.go) ---
	texts := chunker.Split(doc.Content, chunker.SelectOptions(*doc))
	chunkSlice := make([]store.Chunk, 0, len(texts))
	for i, t := range texts {
		chunkSlice = append(chunkSlice, store.Chunk{
			DocumentID: doc.ID,
			ChunkIndex: i,
			Content:    t,
			ByteSize:   len(t),
		})
	}

	if len(chunkSlice) > 0 {
		if err := s.ingestChunks.ReplaceDocument(r.Context(), doc.ID, chunkSlice); err != nil {
			slog.Error("ingest_file: chunk replace failed", "doc_id", doc.ID, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}

		if s.ingestEmbedder != nil && s.ingestEmbedder.Enabled() {
			if embErr := ingestEmbedChunks(r.Context(), doc.ID, chunkSlice, s.ingestChunks, s.ingestEmbedder); embErr != nil {
				// Non-fatal: document is stored and fully FTS-searchable.
				// Vector search is degraded but the upload succeeded.
				slog.Warn("ingest_file: embedding failed (non-fatal)", "doc_id", doc.ID, "error", embErr)
			}
		}
	}

	writeJSON(w, http.StatusCreated, IngestFileResponse{
		DocumentID: doc.ID.String(),
		Accepted:   true,
	})
}

// errUnsupportedFormat is returned by extractFileText when no extractor
// handles the given extension.
var errUnsupportedFormat = fmt.Errorf("unsupported file format")

// plainTextExts lists file extensions that are treated as UTF-8 plain text
// without invoking a format-specific extractor.
var plainTextExts = map[string]struct{}{
	".txt":  {},
	".md":   {},
	".text": {},
}

// extractFileText returns the plain-text content of the uploaded file.
//
// For extensions in plainTextExts the raw bytes are cast to string directly.
// For all other extensions the extractor.Registry is consulted; the file is
// written to a temporary path so the extractor can open it by path.
// errUnsupportedFormat is returned when neither path applies.
func extractFileText(ctx context.Context, ext, filename string, data []byte) (string, error) {
	if _, ok := plainTextExts[ext]; ok {
		return string(data), nil
	}

	reg := extractor.NewRegistry()
	ex := reg.Find(ext)
	if ex == nil {
		return "", errUnsupportedFormat
	}

	// The extractor API takes a filesystem path. Write a temp file, extract,
	// then clean up.
	tmp, err := os.CreateTemp("", "ingest-*"+ext)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck
		return "", fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temp file: %w", err)
	}

	extractCtx, cancel := context.WithTimeout(ctx, extractor.ExtractTimeout*time.Second)
	defer cancel()

	text, err := ex.Extract(extractCtx, tmpPath)
	if err != nil {
		return "", fmt.Errorf("extract %s: %w", filename, err)
	}
	return text, nil
}

// uploadSourceID returns a stable SourceID for an uploaded file by hashing
// the original filename and raw file content. The first 6 bytes (12 hex
// chars) of the SHA-256 digest are used: ample for de-duplication while
// keeping the value compact.
//
// Format: "upload:<12 hex chars>"
func uploadSourceID(filename string, data []byte) string {
	h := sha256.New()
	_, _ = io.WriteString(h, filename)
	_, _ = h.Write(data)
	return fmt.Sprintf("upload:%x", h.Sum(nil)[:6])
}

// ingestEmbedChunks generates and persists embedding vectors for the given
// chunks. Mirrors embedNoteChunks in cmd/mcp/main.go.
func ingestEmbedChunks(
	ctx context.Context,
	docID uuid.UUID,
	chunkSlice []store.Chunk,
	chunkStore IngestFileChunkWriter,
	embedClient IngestFileEmbedder,
) error {
	texts := make([]string, 0, len(chunkSlice))
	for _, c := range chunkSlice {
		texts = append(texts, c.Content)
	}

	vectors, err := embedClient.EmbedBatch(ctx, texts)
	if err != nil {
		return fmt.Errorf("embed batch: %w", err)
	}

	storedChunks, err := chunkStore.ListByDocument(ctx, docID)
	if err != nil {
		return fmt.Errorf("list stored chunks: %w", err)
	}

	idxToID := make(map[int]int64, len(storedChunks))
	for _, sc := range storedChunks {
		idxToID[sc.ChunkIndex] = sc.ID
	}

	embeddings := make([]store.ChunkEmbedding, 0, len(chunkSlice))
	for i, vec := range vectors {
		if i >= len(chunkSlice) {
			break
		}
		id, ok := idxToID[chunkSlice[i].ChunkIndex]
		if !ok {
			slog.Warn("ingestEmbedChunks: chunk index not found in stored chunks",
				"doc_id", docID,
				"chunk_index", chunkSlice[i].ChunkIndex,
			)
			continue
		}
		embeddings = append(embeddings, store.ChunkEmbedding{
			ChunkID:   id,
			Embedding: vec,
		})
	}

	if len(embeddings) == 0 {
		return nil
	}
	return chunkStore.UpdateChunkEmbeddings(ctx, embeddings)
}

// isMaxBytesError reports whether err signals that an http.MaxBytesReader
// limit was exceeded.
func isMaxBytesError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "request body too large")
}

// Ensure multipart.File implements io.ReadCloser so compiler catches drift.
var _ io.ReadCloser = (multipart.File)(nil)
