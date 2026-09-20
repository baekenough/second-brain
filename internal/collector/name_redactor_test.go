package collector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/config"
	"github.com/baekenough/second-brain/internal/llm"
	"github.com/baekenough/second-brain/internal/model"
)

type nameTestClient struct {
	calls    int
	fail     bool
	response string
}

func (f *nameTestClient) Enabled() bool { return true }
func (f *nameTestClient) CompleteWithMessages(ctx context.Context, _ string, m []llm.Message) (string, error) {
	f.calls++
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if f.fail {
		return "", errors.New("private failure")
	}
	if f.response != "" {
		return f.response, nil
	}
	spans := []map[string]string{}
	for _, v := range []struct{ kind, text string }{{"PERSON", "홍길동"}, {"PERSON", "Alice Example"}, {"PHONE", "공일공 일이삼사 오육칠팔"}} {
		if strings.Contains(m[0].Content, v.text) {
			spans = append(spans, map[string]string{"type": v.kind, "text": v.text})
		}
	}
	data, _ := json.Marshal(map[string]any{"complete": true, "spans": spans})
	return string(data), nil
}

func TestNameRedactorCoverageAndKnownFields(t *testing.T) {
	client := &nameTestClient{}
	r, _ := NewNameRedactor(client)
	doc := model.Document{SourceID: "unchanged", Title: "홍길동 통화", Content: strings.Repeat("가", 2990) + "홍길동 공일공 일이삼사 오육칠팔 Alice Example 가격 3000원", Metadata: map[string]any{"contact_name": "Known Contact", "number": "01012345678"}}
	if err := r.Redact(context.Background(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"홍길동", "Alice Example", "공일공 일이삼사 오육칠팔"} {
		if strings.Contains(doc.Title+doc.Content, secret) {
			t.Fatalf("unmasked fixture %q", secret)
		}
	}
	if client.calls < 2 || !strings.Contains(doc.Content, "3000원") || doc.SourceID != "unchanged" || doc.Metadata["contact_name"] != "[REDACTED]" {
		t.Fatal("coverage or identity failure")
	}
}
func TestNameRedactorRejectsInvalidAndOversizeWithoutMutation(t *testing.T) {
	for _, response := range []string{"{", `{"complete":false,"spans":[]}`, `{"complete":true}`, `{"complete":true,"spans":[{"type":"PERSON","text":"invented"}]}`, `{"complete":true,"spans":[],"other":true}`} {
		r, _ := NewNameRedactor(&nameTestClient{response: response})
		doc := model.Document{Content: "original", Metadata: map[string]any{"contact_name": "name"}}
		if err := r.Redact(context.Background(), &doc); err == nil {
			t.Fatalf("accepted invalid %s", response)
		}
		if doc.Content != "original" || doc.Metadata["contact_name"] != "name" {
			t.Fatal("mutated failed document")
		}
	}
	client := &nameTestClient{}
	r, _ := NewNameRedactor(client)
	doc := model.Document{Content: strings.Repeat("가", nameWindowRunes*nameMaxWindows+1)}
	if err := r.Redact(context.Background(), &doc); err == nil || client.calls != 0 {
		t.Fatal("oversize must fail before API call")
	}
}
func TestWhisperNameRedactionFailureRetriesUnindexedFile(t *testing.T) {
	dir := t.TempDir()
	srv, _ := newWhisperTestServer(t, "홍길동 공일공 일이삼사 오육칠팔")
	writeDummyAudio(t, dir, "call.m4a", time.Now().Add(-time.Hour))
	cfg := &config.Config{WhisperAudioDir: dir, WhisperAPIURL: srv.URL, WhisperModel: "whisper-1", WhisperLanguage: "ko", PIINameRedactionEnabled: true}
	client := &nameTestClient{fail: true}
	redactor, _ := NewNameRedactor(client)
	c := makeWhisperCollector(cfg, srv).WithNameRedactor(redactor)
	c.WithIndexedIDs(map[string]struct{}{})
	docs, err := c.Collect(context.Background(), time.Now())
	if err != nil || len(docs) != 0 {
		t.Fatalf("failed API emitted document: %v", err)
	}
	client.fail = false
	docs, err = c.Collect(context.Background(), time.Now())
	if err != nil || len(docs) != 1 {
		t.Fatalf("unindexed file did not retry: %v", err)
	}
	if strings.Contains(docs[0].Content, "홍길동") {
		t.Fatal("raw transcript emitted")
	}
	cfg.PIINameRedactionEnabled = false
	client.calls = 0
	docs, err = c.Collect(context.Background(), time.Now())
	if err != nil || len(docs) != 1 || client.calls != 0 || !strings.Contains(docs[0].Content, "홍길동") {
		t.Fatal("disabled mode changed behavior")
	}
}
