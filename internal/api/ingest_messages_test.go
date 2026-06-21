package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/collector/smsmap"
	"github.com/baekenough/second-brain/internal/model"
)

// --- helpers ---

// newMessagesTestServer creates a Server wired for the ingest-messages handler.
// maxBatch 0 uses the package default.
func newMessagesTestServer(
	upserter IngestMessagesUpserter,
	chunks IngestFileChunkWriter,
	embed IngestFileEmbedder,
	maxBatch int,
	cutover time.Time,
) *Server {
	srv := NewServer(nil, nil, nil, nil, nil, "", "test-key")
	srv.WithIngestMessages(upserter, chunks, embed, maxBatch, cutover)
	return srv
}

// doMessagesPost sends a POST /api/v1/ingest/messages through the full chi router.
func doMessagesPost(t *testing.T, srv *Server, body any, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatalf("encode request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/messages", &buf)
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

// Note: stubIngestUpserter, stubIngestChunkWriter, and stubIngestEmbedder are
// defined in ingest_file_test.go (same package) and satisfy the messages
// handler interfaces too, since the interface signatures are identical.

// --- tests ---

// TestIngestMessages_AuthRequired verifies that missing Bearer token returns 401.
func TestIngestMessages_AuthRequired(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv := newMessagesTestServer(upserter, &stubIngestChunkWriter{}, &stubIngestEmbedder{}, 0, time.Time{})

	body := map[string]any{
		"sms":   []any{},
		"calls": []any{},
	}
	rr := doMessagesPost(t, srv, body, "" /* no auth */)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}
}

// TestIngestMessages_EmptyBatch verifies that an empty payload returns 201 with
// accepted=0, skipped=0.
func TestIngestMessages_EmptyBatch(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv := newMessagesTestServer(upserter, &stubIngestChunkWriter{}, &stubIngestEmbedder{}, 0, time.Time{})

	body := map[string]any{
		"sms":   []any{},
		"calls": []any{},
	}
	rr := doMessagesPost(t, srv, body, "Bearer test-key")

	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	var resp IngestMessagesResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Accepted != 0 || resp.Skipped != 0 {
		t.Errorf("accepted=%d skipped=%d, want 0/0", resp.Accepted, resp.Skipped)
	}
	if resp.Errors == nil {
		t.Error("errors should be [] not null")
	}
}

// TestIngestMessages_SMSMapsCorrectly verifies that a valid SMS record is
// upserted with the correct SourceType, SourceID, direction, and is_auth_like.
func TestIngestMessages_SMSMapsCorrectly(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv := newMessagesTestServer(upserter, &stubIngestChunkWriter{}, &stubIngestEmbedder{}, 0, time.Time{})

	addr := "010-1111-2222"
	body := "안녕하세요"
	dateMs := time.Now().Add(-time.Hour).UnixMilli()

	payload := map[string]any{
		"sms": []any{
			map[string]any{
				"address":      addr,
				"body":         body,
				"date_ms":      dateMs,
				"type":         1, // received
				"contact_name": "Alice",
			},
		},
	}
	rr := doMessagesPost(t, srv, payload, "Bearer test-key")

	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}

	var resp IngestMessagesResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Accepted != 1 {
		t.Errorf("accepted=%d, want 1", resp.Accepted)
	}

	if len(upserter.upserted) != 1 {
		t.Fatalf("expected 1 upserted doc, got %d", len(upserter.upserted))
	}
	doc := upserter.upserted[0]

	if doc.SourceType != model.SourceSMS {
		t.Errorf("SourceType=%q, want %q", doc.SourceType, model.SourceSMS)
	}

	// SourceID uses direction (stable) instead of bodyHash since #144.
	// type=1 → direction="received"
	wantSourceID := fmt.Sprintf("sms:%d:%s:received", dateMs,
		smsmap.ShortHash(addr))
	if doc.SourceID != wantSourceID {
		t.Errorf("SourceID=%q, want %q", doc.SourceID, wantSourceID)
	}

	dir, _ := doc.Metadata["direction"].(string)
	if dir != "received" {
		t.Errorf("direction=%q, want received", dir)
	}
	isAuth, _ := doc.Metadata["is_auth_like"].(bool)
	if isAuth {
		t.Error("is_auth_like should be false for non-auth body")
	}
}

// TestIngestMessages_CallMapsCorrectly verifies that a call record is upserted
// with the correct SourceType and metadata.
func TestIngestMessages_CallMapsCorrectly(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv := newMessagesTestServer(upserter, &stubIngestChunkWriter{}, &stubIngestEmbedder{}, 0, time.Time{})

	number := "010-5555-6666"
	dateMs := time.Now().Add(-2 * time.Hour).UnixMilli()
	durationSec := 120

	payload := map[string]any{
		"calls": []any{
			map[string]any{
				"number":       number,
				"date_ms":      dateMs,
				"duration_sec": durationSec,
				"type":         2, // outgoing
				"contact_name": "Bob",
			},
		},
	}
	rr := doMessagesPost(t, srv, payload, "Bearer test-key")

	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}

	var resp IngestMessagesResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Accepted != 1 {
		t.Errorf("accepted=%d, want 1", resp.Accepted)
	}

	if len(upserter.upserted) != 1 {
		t.Fatalf("expected 1 upserted doc, got %d", len(upserter.upserted))
	}
	doc := upserter.upserted[0]

	if doc.SourceType != model.SourceCallLog {
		t.Errorf("SourceType=%q, want %q", doc.SourceType, model.SourceCallLog)
	}

	wantSourceID := fmt.Sprintf("call-log:%d:%s:%s", dateMs,
		smsmap.ShortHash(number),
		smsmap.BodyShortHash(fmt.Sprintf("%d", durationSec)))
	if doc.SourceID != wantSourceID {
		t.Errorf("SourceID=%q, want %q", doc.SourceID, wantSourceID)
	}

	dir, _ := doc.Metadata["direction"].(string)
	if dir != "outgoing" {
		t.Errorf("direction=%q, want outgoing", dir)
	}
	dur, _ := doc.Metadata["duration_seconds"].(int)
	if dur != durationSec {
		t.Errorf("duration_seconds=%d, want %d", dur, durationSec)
	}
}

// TestIngestMessages_CutoverSkipsOldRecords verifies that records with
// OccurredAt before the cutover floor are skipped.
func TestIngestMessages_CutoverSkipsOldRecords(t *testing.T) {
	t.Parallel()

	cutover := time.Now().Add(-30 * time.Minute)

	upserter := &stubIngestUpserter{}
	srv := newMessagesTestServer(upserter, &stubIngestChunkWriter{}, &stubIngestEmbedder{}, 0, cutover)

	oldMs := time.Now().Add(-2 * time.Hour).UnixMilli() // before cutover
	newMs := time.Now().Add(-10 * time.Minute).UnixMilli() // after cutover

	payload := map[string]any{
		"sms": []any{
			map[string]any{
				"address": "010-0000-0001",
				"body":    "old message",
				"date_ms": oldMs,
				"type":    1,
			},
			map[string]any{
				"address": "010-0000-0002",
				"body":    "new message",
				"date_ms": newMs,
				"type":    1,
			},
		},
	}
	rr := doMessagesPost(t, srv, payload, "Bearer test-key")

	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}

	var resp IngestMessagesResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Accepted != 1 {
		t.Errorf("accepted=%d, want 1", resp.Accepted)
	}
	if resp.Skipped != 1 {
		t.Errorf("skipped=%d, want 1", resp.Skipped)
	}
	if len(upserter.upserted) != 1 {
		t.Fatalf("expected 1 upserted doc, got %d", len(upserter.upserted))
	}
}

// TestIngestMessages_Idempotent verifies that uploading the same record twice
// produces the same SourceID (idempotency is enforced by Upsert in the store).
func TestIngestMessages_Idempotent(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv := newMessagesTestServer(upserter, &stubIngestChunkWriter{}, &stubIngestEmbedder{}, 0, time.Time{})

	addr := "010-7777-8888"
	body := "test message"
	dateMs := time.Now().Add(-time.Hour).UnixMilli()
	payload := map[string]any{
		"sms": []any{
			map[string]any{
				"address": addr,
				"body":    body,
				"date_ms": dateMs,
				"type":    1,
			},
		},
	}

	// First request.
	rr1 := doMessagesPost(t, srv, payload, "Bearer test-key")
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first: status = %d, want 201", rr1.Code)
	}
	// Second request (identical).
	rr2 := doMessagesPost(t, srv, payload, "Bearer test-key")
	if rr2.Code != http.StatusCreated {
		t.Fatalf("second: status = %d, want 201", rr2.Code)
	}

	if len(upserter.upserted) != 2 {
		t.Fatalf("expected 2 upsert calls, got %d", len(upserter.upserted))
	}
	// Both calls must use the same SourceID.
	id1 := upserter.upserted[0].SourceID
	id2 := upserter.upserted[1].SourceID
	if id1 != id2 {
		t.Errorf("SourceID mismatch (not idempotent): first=%q second=%q", id1, id2)
	}
}

// TestIngestMessages_OversizedBatch verifies that a batch exceeding the limit
// returns 413.
func TestIngestMessages_OversizedBatch(t *testing.T) {
	t.Parallel()

	// Limit to 2 records.
	srv := newMessagesTestServer(&stubIngestUpserter{}, &stubIngestChunkWriter{}, &stubIngestEmbedder{}, 2, time.Time{})

	// 3 SMS records — exceeds the cap.
	var smsRecords []any
	for i := 0; i < 3; i++ {
		smsRecords = append(smsRecords, map[string]any{
			"address": fmt.Sprintf("010-0000-%04d", i),
			"body":    "hello",
			"date_ms": time.Now().Add(-time.Duration(i) * time.Hour).UnixMilli(),
			"type":    1,
		})
	}
	payload := map[string]any{"sms": smsRecords}
	rr := doMessagesPost(t, srv, payload, "Bearer test-key")

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusRequestEntityTooLarge, rr.Body.String())
	}
}

// TestIngestMessages_InvalidBody verifies that a malformed JSON body returns 400.
func TestIngestMessages_InvalidBody(t *testing.T) {
	t.Parallel()

	srv := newMessagesTestServer(&stubIngestUpserter{}, &stubIngestChunkWriter{}, &stubIngestEmbedder{}, 0, time.Time{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/messages",
		bytes.NewBufferString("not valid json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
}

// TestIngestMessages_MissingAddress verifies that an SMS record without an
// address is counted as an error (not a server-side crash).
func TestIngestMessages_MissingAddress(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv := newMessagesTestServer(upserter, &stubIngestChunkWriter{}, &stubIngestEmbedder{}, 0, time.Time{})

	payload := map[string]any{
		"sms": []any{
			map[string]any{
				// address intentionally omitted
				"body":    "hello",
				"date_ms": time.Now().Add(-time.Hour).UnixMilli(),
				"type":    1,
			},
		},
	}
	rr := doMessagesPost(t, srv, payload, "Bearer test-key")

	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}

	var resp IngestMessagesResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Accepted != 0 {
		t.Errorf("accepted=%d, want 0", resp.Accepted)
	}
	if len(resp.Errors) == 0 {
		t.Error("expected at least one error entry")
	}
}

// TestIngestMessages_DuplicateBatchSkipsEmbed verifies the core performance fix:
// when a batch of records is re-sent and none of them have changed content,
// chunk replacement and embedding are skipped for every unchanged record.
//
// This mirrors the prod scenario: 224 records retried, 216 unchanged → only
// the 8 new/changed records should trigger embed work. Here we test the skip
// with a tracking embedder so we can assert embed call count.
func TestIngestMessages_DuplicateBatchSkipsEmbed(t *testing.T) {
	t.Parallel()

	// funcEmbedder counts EmbedBatch invocations so we can assert that
	// unchanged records never trigger an embed call.
	var embedCallCount int
	embedder := &funcEmbedder{
		enabled: true,
		embedBatch: func(_ context.Context, texts []string) ([][]float32, error) {
			embedCallCount++
			vecs := make([][]float32, len(texts))
			for i := range vecs {
				vecs[i] = []float32{0.1, 0.2, 0.3}
			}
			return vecs, nil
		},
	}

	// Sequence: first two records return contentChanged=false (duplicates),
	// third record returns contentChanged=true (new).
	upserter := &stubIngestUpserter{
		contentChangedSequence: []bool{false, false, true},
	}

	srv := newMessagesTestServer(upserter, &stubIngestChunkWriter{}, embedder, 0, time.Time{})

	payload := map[string]any{
		"sms": []any{
			map[string]any{"address": "010-0000-0001", "body": "dup msg 1", "date_ms": time.Now().Add(-3 * time.Hour).UnixMilli(), "type": 1},
			map[string]any{"address": "010-0000-0002", "body": "dup msg 2", "date_ms": time.Now().Add(-2 * time.Hour).UnixMilli(), "type": 1},
			map[string]any{"address": "010-0000-0003", "body": "new msg", "date_ms": time.Now().Add(-1 * time.Hour).UnixMilli(), "type": 1},
		},
	}
	rr := doMessagesPost(t, srv, payload, "Bearer test-key")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rr.Code, rr.Body.String())
	}
	var resp IngestMessagesResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Accepted != 3 {
		t.Errorf("accepted=%d, want 3", resp.Accepted)
	}

	// EmbedBatch must be called exactly once: only for the 1 changed record.
	// The 2 duplicate records must not trigger any embed call.
	if embedCallCount != 1 {
		t.Errorf("EmbedBatch called %d times, want 1 (only new/changed records should embed)", embedCallCount)
	}
}

// funcEmbedder is an IngestFileEmbedder that delegates to a function, allowing
// tests to count or control EmbedBatch invocations without a full mock library.
type funcEmbedder struct {
	enabled    bool
	embedBatch func(ctx context.Context, texts []string) ([][]float32, error)
}

func (f *funcEmbedder) Enabled() bool { return f.enabled }
func (f *funcEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	return f.embedBatch(ctx, texts)
}

// TestIngestMessages_AuthLikeRedacted verifies that OTP digits in auth-like
// SMS bodies are redacted in the stored document content.
func TestIngestMessages_AuthLikeRedacted(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv := newMessagesTestServer(upserter, &stubIngestChunkWriter{}, &stubIngestEmbedder{}, 0, time.Time{})

	payload := map[string]any{
		"sms": []any{
			map[string]any{
				"address": "010-0000-0001",
				"body":    "인증번호: 123456 입니다",
				"date_ms": time.Now().Add(-time.Hour).UnixMilli(),
				"type":    1,
			},
		},
	}
	rr := doMessagesPost(t, srv, payload, "Bearer test-key")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}

	if len(upserter.upserted) != 1 {
		t.Fatalf("expected 1 upserted doc, got %d", len(upserter.upserted))
	}
	doc := upserter.upserted[0]

	isAuth, _ := doc.Metadata["is_auth_like"].(bool)
	if !isAuth {
		t.Error("is_auth_like should be true for OTP body")
	}
	// OTP digits should be redacted.
	if bytes.Contains([]byte(doc.Content), []byte("123456")) {
		t.Errorf("OTP digits should be redacted in content: %q", doc.Content)
	}
	if !bytes.Contains([]byte(doc.Content), []byte("[REDACTED]")) {
		t.Errorf("expected [REDACTED] in content, got: %q", doc.Content)
	}
}
