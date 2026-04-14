package collector_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/baekenough/second-brain/internal/collector"
	"github.com/baekenough/second-brain/internal/model"
)

// ---------------------------------------------------------------------------
// Stub implementations
// ---------------------------------------------------------------------------

// stubDocStore is an in-memory AttachmentDocumentStore used in tests.
type stubDocStore struct {
	upserted []*model.Document
	err      error
}

func (s *stubDocStore) Upsert(_ context.Context, doc *model.Document) error {
	if s.err != nil {
		return s.err
	}
	s.upserted = append(s.upserted, doc)
	return nil
}

// testAttachment constructs a *discordgo.MessageAttachment for a given filename
// and size, pointing at the supplied URL.
func testAttachment(filename, url string, size int) *discordgo.MessageAttachment {
	return &discordgo.MessageAttachment{
		ID:       "att-id-1",
		Filename: filename,
		Size:     size,
		URL:      url,
	}
}

// testMessage constructs a minimal *discordgo.Message suitable for attachment tests.
func testMessage(id string) *discordgo.Message {
	return &discordgo.Message{
		ID: id,
		Author: &discordgo.User{
			ID:       "user-id-1",
			Username: "testuser",
		},
		Timestamp: time.Now(),
	}
}

// ---------------------------------------------------------------------------
// TestProcessAttachment_SkipsTooLarge
// ---------------------------------------------------------------------------

func TestProcessAttachment_SkipsTooLarge(t *testing.T) {
	t.Parallel()

	docStore := &stubDocStore{}
	c := collector.ExportNewDiscordCollectorForTest(http.DefaultClient, docStore, nil)

	oversizeBytes := 25*1024*1024 + 1 // 1 byte over the 25 MB cap
	att := testAttachment("report.pdf", "http://example.com/report.pdf", oversizeBytes)

	err := collector.ExportProcessAttachment(c, context.Background(), "g1", "ch1", testMessage("m1"), att)
	if err != nil {
		t.Fatalf("expected nil error for oversized attachment, got: %v", err)
	}
	if len(docStore.upserted) != 0 {
		t.Fatalf("expected no documents upserted for oversized attachment, got %d", len(docStore.upserted))
	}
}

// ---------------------------------------------------------------------------
// TestProcessAttachment_SkipsUnsupportedExt
// ---------------------------------------------------------------------------

func TestProcessAttachment_SkipsUnsupportedExt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		filename string
	}{
		{"exe", "malware.exe"},
		{"zip", "archive.zip"},
		{"png", "image.png"},
		{"mp4", "video.mp4"},
		{"no_ext", "README"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			docStore := &stubDocStore{}
			// httptest server should never be called for unsupported extensions.
			var called atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called.Store(true)
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			c := collector.ExportNewDiscordCollectorForTest(srv.Client(), docStore, nil)
			att := testAttachment(tc.filename, srv.URL+"/"+tc.filename, 100)

			err := collector.ExportProcessAttachment(c, context.Background(), "g1", "ch1", testMessage("m1"), att)
			if err != nil {
				t.Fatalf("unexpected error for unsupported ext %q: %v", tc.filename, err)
			}
			if called.Load() {
				t.Errorf("HTTP server was called for unsupported extension %q — expected skip", tc.filename)
			}
			if len(docStore.upserted) != 0 {
				t.Errorf("expected no documents upserted for unsupported ext %q, got %d", tc.filename, len(docStore.upserted))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestProcessAttachment_HTTPFailure
// ---------------------------------------------------------------------------

func TestProcessAttachment_HTTPFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	docStore := &stubDocStore{}
	c := collector.ExportNewDiscordCollectorForTest(srv.Client(), docStore, nil)
	att := testAttachment("notes.txt", srv.URL+"/notes.txt", 50)

	err := collector.ExportProcessAttachment(c, context.Background(), "g1", "ch1", testMessage("m1"), att)
	if err == nil {
		t.Fatal("expected non-nil error when server returns 500")
	}
	if len(docStore.upserted) != 0 {
		t.Fatalf("expected no documents upserted on HTTP failure, got %d", len(docStore.upserted))
	}
}

// ---------------------------------------------------------------------------
// TestProcessAttachment_PlainText_Success
// ---------------------------------------------------------------------------

func TestProcessAttachment_PlainText_Success(t *testing.T) {
	t.Parallel()

	content := "Hello from Discord attachment\nLine two"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	}))
	defer srv.Close()

	docStore := &stubDocStore{}
	c := collector.ExportNewDiscordCollectorForTest(srv.Client(), docStore, nil)

	for _, ext := range []string{".txt", ".md", ".csv", ".json", ".yaml", ".yml"} {
		t.Run(ext, func(t *testing.T) {
			docStore.upserted = nil
			att := testAttachment("file"+ext, srv.URL+"/file"+ext, len(content))

			err := collector.ExportProcessAttachment(c, context.Background(), "g1", "ch1", testMessage("m1"), att)
			if err != nil {
				t.Fatalf("unexpected error for %s: %v", ext, err)
			}
			if len(docStore.upserted) != 1 {
				t.Fatalf("expected 1 upserted document for %s, got %d", ext, len(docStore.upserted))
			}
			doc := docStore.upserted[0]
			if doc.SourceType != "discord" {
				t.Errorf("source_type: want %q, got %q", "discord", doc.SourceType)
			}
			if doc.Content != content {
				t.Errorf("content mismatch: want %q, got %q", content, doc.Content)
			}
			// SourceID must contain attachment ID.
			if !containsString(doc.SourceID, "att-id-1") {
				t.Errorf("source_id %q does not contain attachment ID", doc.SourceID)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestProcessAttachment_NilDocStore_Noop
// ---------------------------------------------------------------------------

func TestProcessAttachment_NilDocStore_Noop(t *testing.T) {
	t.Parallel()

	// When docStore is nil, processAttachment must return nil without downloading anything.
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := collector.ExportNewDiscordCollectorForTest(srv.Client(), nil, nil)
	att := testAttachment("notes.txt", srv.URL+"/notes.txt", 50)

	err := collector.ExportProcessAttachment(c, context.Background(), "g1", "ch1", testMessage("m1"), att)
	if err != nil {
		t.Fatalf("expected nil error with nil docStore, got: %v", err)
	}
	if called.Load() {
		t.Error("HTTP server was called despite nil docStore — expected early return")
	}
}

// ---------------------------------------------------------------------------
// TestProcessAttachment_SourceID_Format
// ---------------------------------------------------------------------------

func TestProcessAttachment_SourceID_Format(t *testing.T) {
	t.Parallel()

	content := "# Markdown content"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	}))
	defer srv.Close()

	docStore := &stubDocStore{}
	c := collector.ExportNewDiscordCollectorForTest(srv.Client(), docStore, nil)

	msg := testMessage("msg-42")
	att := testAttachment("doc.md", srv.URL+"/doc.md", len(content))

	if err := collector.ExportProcessAttachment(c, context.Background(), "guild-99", "chan-7", msg, att); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(docStore.upserted) != 1 {
		t.Fatalf("expected 1 document, got %d", len(docStore.upserted))
	}

	want := "discord:guild-99:chan-7:msg-42:att:att-id-1"
	got := docStore.upserted[0].SourceID
	if got != want {
		t.Errorf("source_id: want %q, got %q", want, got)
	}
}

// ---------------------------------------------------------------------------
// TestProcessAttachment_Metadata_Fields
// ---------------------------------------------------------------------------

func TestProcessAttachment_Metadata_Fields(t *testing.T) {
	t.Parallel()

	content := "data"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	}))
	defer srv.Close()

	docStore := &stubDocStore{}
	c := collector.ExportNewDiscordCollectorForTest(srv.Client(), docStore, nil)

	msg := &discordgo.Message{
		ID: "msg-1",
		Author: &discordgo.User{
			ID:       "user-99",
			Username: "alice",
		},
		Timestamp: time.Now(),
	}
	att := &discordgo.MessageAttachment{
		ID:          "att-99",
		Filename:    "data.csv",
		Size:        len(content),
		URL:         srv.URL + "/data.csv",
		ContentType: "text/csv",
	}

	if err := collector.ExportProcessAttachment(c, context.Background(), "gld", "chl", msg, att); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(docStore.upserted) != 1 {
		t.Fatalf("expected 1 document, got %d", len(docStore.upserted))
	}

	meta := docStore.upserted[0].Metadata
	assertMeta := func(key, want string) {
		t.Helper()
		got, ok := meta[key]
		if !ok {
			t.Errorf("metadata missing key %q", key)
			return
		}
		if got != want {
			t.Errorf("metadata[%q]: want %q, got %v", key, want, got)
		}
	}
	assertMeta("guild_id", "gld")
	assertMeta("channel_id", "chl")
	assertMeta("message_id", "msg-1")
	assertMeta("attachment_id", "att-99")
	assertMeta("filename", "data.csv")
	assertMeta("content_type", "text/csv")
	assertMeta("author_id", "user-99")
	assertMeta("author_name", "alice")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func containsString(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsRaw(s, sub))
}

func containsRaw(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
