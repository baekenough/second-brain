package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/model"
	"github.com/baekenough/second-brain/internal/store"
)

// --- stubs ---

type stubIngestUpserter struct {
	upserted []*model.Document
	err      error
	// contentChangedSequence controls the contentChanged value returned by
	// UpsertTracked for each successive call. When the slice is exhausted every
	// further call returns true (new document). Use this to simulate a duplicate
	// batch where some records are unchanged.
	contentChangedSequence []bool
	callCount              int
}

func (s *stubIngestUpserter) Upsert(_ context.Context, doc *model.Document) error {
	if s.err != nil {
		return s.err
	}
	// Simulate ID assignment (store does RETURNING id).
	if doc.ID == uuid.Nil {
		doc.ID = uuid.New()
	}
	s.upserted = append(s.upserted, doc)
	return nil
}

// UpsertTracked satisfies IngestMessagesUpserter. It behaves identically to
// Upsert for document persistence. The contentChanged return value is driven by
// contentChangedSequence: if the slice has an entry for this call index it is
// used; otherwise true is returned (default: treat as new/changed document).
func (s *stubIngestUpserter) UpsertTracked(_ context.Context, doc *model.Document) (contentChanged bool, err error) {
	if s.err != nil {
		return false, s.err
	}
	if doc.ID == uuid.Nil {
		doc.ID = uuid.New()
	}
	s.upserted = append(s.upserted, doc)

	idx := s.callCount
	s.callCount++
	if idx < len(s.contentChangedSequence) {
		return s.contentChangedSequence[idx], nil
	}
	return true, nil // default: content is new/changed
}

type stubIngestChunkWriter struct {
	replaced []uuid.UUID
	err      error
}

func (s *stubIngestChunkWriter) ReplaceDocument(_ context.Context, documentID uuid.UUID, _ []store.Chunk) error {
	if s.err != nil {
		return s.err
	}
	s.replaced = append(s.replaced, documentID)
	return nil
}

func (s *stubIngestChunkWriter) ListByDocument(_ context.Context, _ uuid.UUID) ([]store.Chunk, error) {
	return nil, nil
}

func (s *stubIngestChunkWriter) UpdateChunkEmbeddings(_ context.Context, _ []store.ChunkEmbedding) error {
	return nil
}

type stubIngestEmbedder struct {
	enabled bool
}

func (s *stubIngestEmbedder) Enabled() bool { return s.enabled }
func (s *stubIngestEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	vecs := make([][]float32, len(texts))
	for i := range vecs {
		vecs[i] = []float32{0.1, 0.2, 0.3}
	}
	return vecs, nil
}

// --- helpers ---

// newIngestTestServer creates a Server wired for the ingest-file handler.
// maxFileBytes 0 uses the package default.
func newIngestTestServer(upserter IngestFileUpserter, chunks IngestFileChunkWriter, embed IngestFileEmbedder, maxFileBytes int64) *Server {
	srv := NewServer(nil, nil, nil, nil, nil, "", "test-key")
	srv.WithIngestFile(upserter, chunks, embed, maxFileBytes)
	return srv
}

// buildMultipart constructs a multipart/form-data request body with a single
// "file" part whose content is data. Optional form fields can be supplied via
// extra (alternating key/value pairs).
func buildMultipart(t *testing.T, filename string, data []byte, extra ...string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// File part.
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatalf("write form file: %v", err)
	}

	// Extra form fields.
	for i := 0; i+1 < len(extra); i += 2 {
		if err := mw.WriteField(extra[i], extra[i+1]); err != nil {
			t.Fatalf("write form field %s: %v", extra[i], err)
		}
	}

	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// doIngestPost sends a POST /api/v1/ingest/file through the full chi router.
func doIngestPost(t *testing.T, srv *Server, body io.Reader, contentType, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/file", body)
	req.Header.Set("Content-Type", contentType)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

// --- tests ---

// TestIngestFile_AuthRequired verifies that missing Bearer token returns 401.
func TestIngestFile_AuthRequired(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	chunks := &stubIngestChunkWriter{}
	srv := newIngestTestServer(upserter, chunks, &stubIngestEmbedder{}, 0)

	body, ct := buildMultipart(t, "hello.txt", []byte("hello world"))
	rr := doIngestPost(t, srv, body, ct, "" /* no auth */)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}
}

// TestIngestFile_SuccessText verifies that uploading a .txt file creates a
// document and returns 201 with document_id and accepted=true.
func TestIngestFile_SuccessText(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	chunks := &stubIngestChunkWriter{}
	srv := newIngestTestServer(upserter, chunks, &stubIngestEmbedder{}, 0)

	content := []byte("This is a sample document used for testing the ingest endpoint.")
	body, ct := buildMultipart(t, "sample.txt", content,
		"title", "My Test Doc",
		"source", "test-suite",
		"tags", "go,test,ingest",
	)
	rr := doIngestPost(t, srv, body, ct, "Bearer test-key")

	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}

	var resp IngestFileResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Accepted {
		t.Error("expected accepted=true")
	}
	if resp.DocumentID == "" {
		t.Error("expected non-empty document_id")
	}
	if _, err := uuid.Parse(resp.DocumentID); err != nil {
		t.Errorf("document_id %q is not a valid UUID: %v", resp.DocumentID, err)
	}

	// Verify the document was upserted with the correct source type.
	if len(upserter.upserted) != 1 {
		t.Fatalf("expected 1 upserted document, got %d", len(upserter.upserted))
	}
	doc := upserter.upserted[0]
	if doc.SourceType != model.SourceUpload {
		t.Errorf("source_type = %q, want %q", doc.SourceType, model.SourceUpload)
	}
	if doc.Title != "My Test Doc" {
		t.Errorf("title = %q, want %q", doc.Title, "My Test Doc")
	}
	if !bytes.Contains([]byte(doc.Content), []byte("sample document")) {
		t.Errorf("content does not contain expected text: %q", doc.Content)
	}
}

// TestIngestFile_OversizedFile verifies that a file exceeding the size limit
// returns 413.
func TestIngestFile_OversizedFile(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	chunks := &stubIngestChunkWriter{}
	// Limit to 64 bytes.
	srv := newIngestTestServer(upserter, chunks, &stubIngestEmbedder{}, 64)

	// Upload a file larger than 64 bytes.
	oversized := bytes.Repeat([]byte("x"), 128)
	body, ct := buildMultipart(t, "big.txt", oversized)
	rr := doIngestPost(t, srv, body, ct, "Bearer test-key")

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusRequestEntityTooLarge, rr.Body.String())
	}

	var errBody map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if _, ok := errBody["error"]; !ok {
		t.Error("response missing 'error' key")
	}
}

// TestIngestFile_UnsupportedType verifies that an unknown extension returns 415.
func TestIngestFile_UnsupportedType(t *testing.T) {
	t.Parallel()

	srv := newIngestTestServer(&stubIngestUpserter{}, &stubIngestChunkWriter{}, &stubIngestEmbedder{}, 0)

	body, ct := buildMultipart(t, "archive.zip", []byte("PK\x03\x04fake zip content"))
	rr := doIngestPost(t, srv, body, ct, "Bearer test-key")

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusUnsupportedMediaType, rr.Body.String())
	}

	var errBody map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if _, ok := errBody["error"]; !ok {
		t.Error("response missing 'error' key")
	}
}

// TestIngestFile_Idempotent verifies that uploading the same file twice
// produces the same SourceID both times, demonstrating idempotency.
func TestIngestFile_Idempotent(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	chunks := &stubIngestChunkWriter{}
	srv := newIngestTestServer(upserter, chunks, &stubIngestEmbedder{}, 0)

	filename := "notes.txt"
	content := []byte("Idempotency test: same file uploaded twice should produce the same SourceID.")

	// First upload.
	body1, ct1 := buildMultipart(t, filename, content)
	rr1 := doIngestPost(t, srv, body1, ct1, "Bearer test-key")
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first upload: status = %d, want %d; body: %s", rr1.Code, http.StatusCreated, rr1.Body.String())
	}

	// Second upload (identical).
	body2, ct2 := buildMultipart(t, filename, content)
	rr2 := doIngestPost(t, srv, body2, ct2, "Bearer test-key")
	if rr2.Code != http.StatusCreated {
		t.Fatalf("second upload: status = %d, want %d; body: %s", rr2.Code, http.StatusCreated, rr2.Body.String())
	}

	// Both upsert calls must have used the same SourceID.
	if len(upserter.upserted) != 2 {
		t.Fatalf("expected 2 upsert calls, got %d", len(upserter.upserted))
	}
	id1 := upserter.upserted[0].SourceID
	id2 := upserter.upserted[1].SourceID
	if id1 != id2 {
		t.Errorf("source_id mismatch: first=%q second=%q — uploads are not idempotent", id1, id2)
	}
	// The SourceID should be prefixed with "upload:".
	if len(id1) < 7 || id1[:7] != "upload:" {
		t.Errorf("source_id %q does not have expected 'upload:' prefix", id1)
	}
}

// TestIngestFile_MissingFilePart verifies that omitting the 'file' field
// returns 400.
func TestIngestFile_MissingFilePart(t *testing.T) {
	t.Parallel()

	srv := newIngestTestServer(&stubIngestUpserter{}, &stubIngestChunkWriter{}, &stubIngestEmbedder{}, 0)

	// Send a multipart form with only a non-file field.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("title", "No file here")
	_ = mw.Close()

	rr := doIngestPost(t, srv, &buf, mw.FormDataContentType(), "Bearer test-key")

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
}

// TestIngestFile_DefaultTitleFromFilename verifies that when no title is
// provided the document title is set to the original filename.
func TestIngestFile_DefaultTitleFromFilename(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv := newIngestTestServer(upserter, &stubIngestChunkWriter{}, &stubIngestEmbedder{}, 0)

	body, ct := buildMultipart(t, "readme.txt", []byte("content here"))
	// No "title" field supplied.
	rr := doIngestPost(t, srv, body, ct, "Bearer test-key")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	if len(upserter.upserted) != 1 {
		t.Fatalf("expected 1 upserted document, got %d", len(upserter.upserted))
	}
	if upserter.upserted[0].Title != "readme.txt" {
		t.Errorf("title = %q, want %q", upserter.upserted[0].Title, "readme.txt")
	}
}

// TestIngestFile_MarkdownAccepted verifies that .md files (plain text) are
// accepted and stored without invoking the binary extractor.
func TestIngestFile_MarkdownAccepted(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv := newIngestTestServer(upserter, &stubIngestChunkWriter{}, &stubIngestEmbedder{}, 0)

	mdContent := []byte("# Heading\n\nSome **markdown** content.\n")
	body, ct := buildMultipart(t, "notes.md", mdContent)
	rr := doIngestPost(t, srv, body, ct, "Bearer test-key")

	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	if len(upserter.upserted) != 1 {
		t.Fatalf("expected 1 upserted document, got %d", len(upserter.upserted))
	}
	if !bytes.Contains([]byte(upserter.upserted[0].Content), []byte("markdown")) {
		t.Errorf("content %q does not contain 'markdown'", upserter.upserted[0].Content)
	}
}

// --- unit tests for helpers ---

// TestUploadSourceID verifies that uploadSourceID is deterministic and prefixed.
func TestUploadSourceID(t *testing.T) {
	t.Parallel()

	data := []byte("some file content")
	id1 := uploadSourceID("file.txt", data)
	id2 := uploadSourceID("file.txt", data)

	if id1 != id2 {
		t.Errorf("uploadSourceID is not deterministic: %q != %q", id1, id2)
	}
	if len(id1) < 7 || id1[:7] != "upload:" {
		t.Errorf("uploadSourceID missing 'upload:' prefix: %q", id1)
	}

	// Different filename → different ID.
	id3 := uploadSourceID("other.txt", data)
	if id1 == id3 {
		t.Errorf("uploadSourceID should differ for different filenames: both=%q", id1)
	}

	// Different content → different ID.
	id4 := uploadSourceID("file.txt", []byte("different content"))
	if id1 == id4 {
		t.Errorf("uploadSourceID should differ for different content: both=%q", id1)
	}
}

// TestExtractFileText_PlainText verifies that .txt, .md, and .text extensions
// are returned verbatim without invoking the extractor registry.
func TestExtractFileText_PlainText(t *testing.T) {
	t.Parallel()

	cases := []struct {
		ext  string
		data []byte
	}{
		{".txt", []byte("plain text content")},
		{".md", []byte("# heading\nparagraph")},
		{".text", []byte("some text")},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.ext, func(t *testing.T) {
			t.Parallel()
			got, err := extractFileText(t.Context(), tc.ext, "file"+tc.ext, tc.data)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != string(tc.data) {
				t.Errorf("got %q, want %q", got, string(tc.data))
			}
		})
	}
}

// TestExtractFileText_Unsupported verifies that an unknown extension returns
// errUnsupportedFormat.
func TestExtractFileText_Unsupported(t *testing.T) {
	t.Parallel()

	_, err := extractFileText(t.Context(), ".exe", "binary.exe", []byte{0x00, 0x01})
	if err != errUnsupportedFormat {
		t.Errorf("expected errUnsupportedFormat, got %v", err)
	}
}

// TestIsMaxBytesError validates the isMaxBytesError helper.
func TestIsMaxBytesError(t *testing.T) {
	t.Parallel()

	if isMaxBytesError(nil) {
		t.Error("isMaxBytesError(nil) should be false")
	}
	if isMaxBytesError(fmt.Errorf("some other error")) {
		t.Error("isMaxBytesError should be false for non-size errors")
	}
	if !isMaxBytesError(fmt.Errorf("http: request body too large")) {
		t.Error("isMaxBytesError should be true for MaxBytesError")
	}
}
