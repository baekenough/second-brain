package jev

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withServer points a Client at srv and speeds up retry backoff for the
// duration of the test.
func withServer(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	t.Cleanup(func() { srv.Close() })
	c := New("test-key", srv.Client())
	c.baseURL = srv.URL
	return c
}

func fastBackoff(t *testing.T) {
	t.Helper()
	orig := retryBackoffUnit
	retryBackoffUnit = time.Millisecond
	t.Cleanup(func() { retryBackoffUnit = orig })
}

func TestClient_Enabled(t *testing.T) {
	if (&Client{}).Enabled() {
		t.Error("zero-value client must report disabled")
	}
	if New("", nil).Enabled() {
		t.Error("empty API key must report disabled")
	}
	if !New("k", nil).Enabled() {
		t.Error("non-empty API key must report enabled")
	}
	var nilClient *Client
	if nilClient.Enabled() {
		t.Error("nil client must report disabled")
	}
}

// TestClient_Classify_Success verifies the request shape (auth header,
// method, content type) and that a valid response decodes into usable
// Choice/Score answers plus Usage.
func TestClient_Classify_Success(t *testing.T) {
	var gotAuth, gotMethod, gotContentType string
	var gotBody request

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("server: decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"answers": {
				"segment": {"choice": "personal_comm", "probabilities": {"personal_comm": 0.7, "ad": 0.3}, "confidence": 0.7},
				"retention": {"score": 1.6, "legend": {}, "confidence": 0.5}
			},
			"usage": {"input_tokens": 42}
		}`))
	}))
	c := withServer(t, srv)

	resp, err := c.Classify(context.Background(), "문자 내용: 안녕하세요", map[string]Question{
		"segment":   {Type: QuestionChoice, Instructions: "고르세요", Criteria: map[string]string{"personal_comm": "지인"}},
		"retention": {Type: QuestionScore, Instructions: "점수", Criteria: []string{"a", "b", "c"}},
	})
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer test-key")
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody.Model != defaultModel {
		t.Errorf("request model = %q, want %q", gotBody.Model, defaultModel)
	}
	if gotBody.State != "문자 내용: 안녕하세요" {
		t.Errorf("request state = %q", gotBody.State)
	}

	segment, err := resp.Choice("segment")
	if err != nil {
		t.Fatalf("Choice(segment) error = %v", err)
	}
	if segment.Choice != "personal_comm" {
		t.Errorf("segment.Choice = %q, want personal_comm", segment.Choice)
	}
	if segment.Probability() != 0.7 {
		t.Errorf("segment.Probability() = %v, want 0.7", segment.Probability())
	}

	retention, err := resp.Score("retention")
	if err != nil {
		t.Fatalf("Score(retention) error = %v", err)
	}
	if retention.Score != 1.6 {
		t.Errorf("retention.Score = %v, want 1.6", retention.Score)
	}
	if resp.Usage.InputTokens != 42 {
		t.Errorf("Usage.InputTokens = %d, want 42", resp.Usage.InputTokens)
	}
}

// TestChoiceAnswer_Probability_FallsBackToProbabilitiesMap covers the case
// where the response omits the top-level confidence field.
func TestChoiceAnswer_Probability_FallsBackToProbabilitiesMap(t *testing.T) {
	a := ChoiceAnswer{Choice: "ad", Probabilities: map[string]float64{"ad": 0.95}}
	if got := a.Probability(); got != 0.95 {
		t.Errorf("Probability() = %v, want 0.95 (fallback to probabilities map)", got)
	}
}

func TestChoiceAnswer_Probability_ZeroWhenBothMissing(t *testing.T) {
	a := ChoiceAnswer{Choice: "ad"}
	if got := a.Probability(); got != 0 {
		t.Errorf("Probability() = %v, want 0 when neither confidence nor probabilities is set", got)
	}
}

// TestClient_Classify_RetriesOn429ThenSucceeds verifies the retry loop
// recovers from a transient 429.
func TestClient_Classify_RetriesOn429ThenSucceeds(t *testing.T) {
	fastBackoff(t)
	var calls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"segment":{"choice":"ad","confidence":0.99},"retention":{"score":0}},"usage":{"input_tokens":1}}`))
	}))
	c := withServer(t, srv)

	resp, err := c.Classify(context.Background(), "state", map[string]Question{
		"segment":   {Type: QuestionChoice},
		"retention": {Type: QuestionScore},
	})
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("server received %d requests, want 3 (2 failures + 1 success)", got)
	}
	if choice, _ := resp.Choice("segment"); choice.Choice != "ad" {
		t.Errorf("segment.Choice = %q, want ad", choice.Choice)
	}
}

// TestClient_Classify_ExhaustsRetriesOn5xx verifies that a persistently
// failing server yields an error after exactly maxRetries attempts.
func TestClient_Classify_ExhaustsRetriesOn5xx(t *testing.T) {
	fastBackoff(t)
	var calls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	c := withServer(t, srv)

	_, err := c.Classify(context.Background(), "state", map[string]Question{
		"segment":   {Type: QuestionChoice},
		"retention": {Type: QuestionScore},
	})
	if err == nil {
		t.Fatal("Classify() error = nil, want error after exhausting retries")
	}
	if got := atomic.LoadInt32(&calls); got != maxRetries {
		t.Errorf("server received %d requests, want %d (maxRetries)", got, maxRetries)
	}
}

// TestClient_Classify_NonRetryableStatus_NoRetry verifies a 400 (client
// error, not rate-limit/server-error) is not retried.
func TestClient_Classify_NonRetryableStatus_NoRetry(t *testing.T) {
	fastBackoff(t)
	var calls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	c := withServer(t, srv)

	_, err := c.Classify(context.Background(), "state", map[string]Question{
		"segment":   {Type: QuestionChoice},
		"retention": {Type: QuestionScore},
	})
	if err == nil {
		t.Fatal("Classify() error = nil, want error on 400")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("server received %d requests, want 1 (no retry on 400)", got)
	}
}

// TestClient_Classify_DisabledClient verifies Classify refuses to call out
// when no API key is configured, without making any network request.
func TestClient_Classify_DisabledClient(t *testing.T) {
	c := New("", nil)
	_, err := c.Classify(context.Background(), "state", map[string]Question{})
	if err == nil {
		t.Fatal("Classify() error = nil, want error for disabled client")
	}
}

// TestResponse_Choice_MissingAnswer and TestResponse_Score_MissingAnswer
// cover the "Jev response is missing an expected question" failure mode,
// which internal/classify.ClassifyWithJev must surface as a classification
// failure (retry-eligible), not silently substitute a default.
func TestResponse_Choice_MissingAnswer(t *testing.T) {
	r := &Response{Answers: map[string]json.RawMessage{}}
	if _, err := r.Choice("segment"); err == nil {
		t.Error("Choice() error = nil, want error for missing answer")
	}
}

func TestResponse_Score_MissingAnswer(t *testing.T) {
	r := &Response{Answers: map[string]json.RawMessage{}}
	if _, err := r.Score("retention"); err == nil {
		t.Error("Score() error = nil, want error for missing answer")
	}
}

func TestResponse_Choice_EmptyChoiceIsError(t *testing.T) {
	r := &Response{Answers: map[string]json.RawMessage{
		"segment": json.RawMessage(`{"choice": ""}`),
	}}
	if _, err := r.Choice("segment"); err == nil {
		t.Error("Choice() error = nil, want error for empty choice field")
	}
}

// TestClient_Classify_NeverLeaksBodyInError guards the "never log/error raw
// request or response content" policy (personal message text may be in
// either body).
func TestClient_Classify_NeverLeaksBodyInError(t *testing.T) {
	fastBackoff(t)
	const secretBody = "인증번호는 123456 입니다 -- 이 값이 에러 메시지에 노출되면 안 됨"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(secretBody))
	}))
	c := withServer(t, srv)

	_, err := c.Classify(context.Background(), secretBody, map[string]Question{
		"segment":   {Type: QuestionChoice},
		"retention": {Type: QuestionScore},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secretBody) || strings.Contains(err.Error(), "123456") {
		t.Errorf("error message leaks response/request body content: %v", err)
	}
}
