package llm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/llm"
)

// Both returned errors and observability sinks must omit echoed source/token data.
func TestProviderErrorsDoNotExposeSecrets(t *testing.T) {
	const secret = "private-calendar-source-and-token"
	for _, streaming := range []bool{false, true} {
		for _, status := range []int{400, 500, 200} {
			t.Run(fmt.Sprintf("stream=%v/status=%d", streaming, status), func(t *testing.T) {
				exporter := withInMemoryTracer(t)
				var logs bytes.Buffer
				old := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
				t.Cleanup(func() { slog.SetDefault(old) })
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(status)
					if status == 200 && streaming {
						fmt.Fprintf(w, "data: {\"error\":{\"message\":\"%s\"}}\n\n", secret)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": secret}})
				}))
				defer server.Close()
				client := newClient(t, server.URL, secret)
				var err error
				if streaming {
					err = client.StreamWithMessages(context.Background(), "system", []llm.Message{{Role: "user", Content: secret}}, func(string) { t.Error("unexpected content") })
				} else {
					_, err = client.Complete(context.Background(), "system", secret)
				}
				if err == nil {
					t.Fatal("expected upstream error")
				}
				spans, _ := json.Marshal(exporter.GetSpans())
				for sink, value := range map[string]string{"error": err.Error(), "logs": logs.String(), "spans": string(spans)} {
					if strings.Contains(value, secret) {
						t.Errorf("%s leaked secret", sink)
					}
				}
			})
		}
	}
}

type privateErrorTransport struct{ err error }

func (tr privateErrorTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, tr.err }

func TestTransportErrorsArePrivateAndPreserveCancellation(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, cause := range []error{errors.New("private-transport-secret"), context.Canceled, context.DeadlineExceeded} {
			client := llm.New(llm.Config{BaseURL: "https://example.invalid", Model: "test", APIKey: "test"}, &http.Client{Transport: privateErrorTransport{cause}})
			var err error
			if streaming {
				err = client.StreamWithMessages(context.Background(), "system", nil, func(string) {})
			} else {
				_, err = client.Complete(context.Background(), "system", "user")
			}
			if err == nil || strings.Contains(err.Error(), "private-transport-secret") {
				t.Fatalf("unsafe error %v", err)
			}
			if (cause == context.Canceled || cause == context.DeadlineExceeded) && !errors.Is(err, cause) {
				t.Fatalf("lost cancellation: %v", err)
			}
		}
	}
}
