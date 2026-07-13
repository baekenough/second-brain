package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/baekenough/second-brain/internal/config"
)

// recordingRoundTripper is an http.RoundTripper that records whether it was
// ever invoked and always fails. It is used to prove — at the transport level
// — that diarizeAudio never places a single byte on the wire when the
// diarization endpoint is non-local (issue #165): if the guard were ever
// bypassed, RoundTrip would be called and the test would fail immediately
// rather than silently succeeding via a real (and possibly slow or
// network-dependent) dial attempt.
type recordingRoundTripper struct {
	called bool
}

func (rt *recordingRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	rt.called = true
	return nil, fmt.Errorf("recordingRoundTripper: transport must not be invoked for a non-local diarization endpoint")
}

// TestDiarizeAudio_RefusesNonLocalEndpoint verifies that diarizeAudio returns
// an error — without ever invoking the HTTP transport — when
// cfg.DiarizationAPIURL does not resolve to a loopback/private address.
func TestDiarizeAudio_RefusesNonLocalEndpoint(t *testing.T) {
	t.Parallel()

	rt := &recordingRoundTripper{}
	cfg := &config.Config{
		WhisperModel:       "whisper-1",
		DiarizationEnabled: true,
		DiarizationAPIURL:  "http://8.8.8.8:9999", // public IP — must never be dialled
	}
	c := NewWhisperCollector(cfg)
	c.httpClient = &http.Client{Transport: rt}

	segs, err := c.diarizeAudio(context.Background(), "call.m4a", []byte("fake-audio-bytes"), false)
	if err == nil {
		t.Fatal("diarizeAudio() error = nil, want non-nil for non-local endpoint")
	}
	if segs != nil {
		t.Errorf("diarizeAudio() segments = %v, want nil", segs)
	}
	if rt.called {
		t.Error("HTTP transport was invoked for a non-local diarization endpoint — locality guard did not stop the call (#165)")
	}
}

// TestDiarizeAudio_AllowsLocalEndpoint is a control test proving the locality
// guard does not reject legitimate local endpoints (loopback host from an
// httptest.Server), so the refusal above is specific to non-local hosts and
// not an overly broad guard that breaks the feature entirely.
func TestDiarizeAudio_AllowsLocalEndpoint(t *testing.T) {
	t.Parallel()

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(diarizeResponse{
			Segments: []diarSegment{{Start: 0, End: 1, Speaker: "SPEAKER_00"}},
		})
	}))
	defer srv.Close()

	cfg := &config.Config{
		WhisperModel:       "whisper-1",
		DiarizationEnabled: true,
		DiarizationAPIURL:  srv.URL,
	}
	c := NewWhisperCollector(cfg)
	c.httpClient = srv.Client()

	segs, err := c.diarizeAudio(context.Background(), "call.m4a", []byte("fake-audio-bytes"), false)
	if err != nil {
		t.Fatalf("diarizeAudio() error = %v, want nil for local endpoint", err)
	}
	if len(segs) != 1 {
		t.Errorf("diarizeAudio() segments = %v, want 1 segment", segs)
	}
	if !called {
		t.Error("diarization server was never called for a local endpoint")
	}
}

// TestDiarizeAudio_RefusesRedirect verifies that when a "local" diarization
// endpoint (passing isLocalWhisperEndpoint) responds with an HTTP redirect,
// the shared httpClient's CheckRedirect guard refuses to follow it — so the
// redirect target is NEVER dialled, even though it points at a different
// (potentially non-local) host. This closes the gap where a locally-approved
// endpoint could redirect audio bytes off-box after the locality check has
// already passed (issue #165 follow-up).
//
// The collector under test is constructed via NewWhisperCollector and its
// httpClient is deliberately left untouched (unlike makeWhisperCollector,
// which overrides httpClient with srv.Client() and would silently bypass the
// guard under test).
func TestDiarizeAudio_RefusesRedirect(t *testing.T) {
	t.Parallel()

	redirectTargetHits := 0
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectTargetHits++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(diarizeResponse{
			Segments: []diarSegment{{Start: 0, End: 1, Speaker: "SPEAKER_00"}},
		})
	}))
	defer redirectTarget.Close()

	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+"/diarize", http.StatusFound)
	}))
	defer localSrv.Close()

	cfg := &config.Config{
		WhisperModel:       "whisper-1",
		DiarizationEnabled: true,
		DiarizationAPIURL:  localSrv.URL, // loopback — passes the locality guard
	}
	c := NewWhisperCollector(cfg)
	c.httpClient.Timeout = 5 * time.Second // bound the test if the guard regresses

	segs, err := c.diarizeAudio(context.Background(), "call.m4a", []byte("fake-audio-bytes"), false)
	if err == nil {
		t.Fatal("diarizeAudio() error = nil, want non-nil when the local endpoint issues a redirect")
	}
	if segs != nil {
		t.Errorf("diarizeAudio() segments = %v, want nil", segs)
	}
	if redirectTargetHits != 0 {
		t.Errorf("redirect target was hit %d times, want 0 — CheckRedirect must stop the client before a second request is made", redirectTargetHits)
	}
}

// TestWhisperCollector_DiarizationRefusesNonLocalEndpoint_IntegrationFallback
// exercises the full Collect() path: a non-local DiarizationAPIURL must never
// be dialled and the resulting document must gracefully fall back to the
// plain (non-diarized) transcript, matching the existing diarization-failure
// degrade path.
func TestWhisperCollector_DiarizationRefusesNonLocalEndpoint_IntegrationFallback(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const wantFallbackText = "비로컬 다이어라이제이션 엔드포인트 폴백 확인"

	whisperSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(whisperVerboseResponse{
			Text: wantFallbackText,
			Segments: []whisperSegment{
				{Start: 0.0, End: 3.0, Text: wantFallbackText},
			},
		})
	}))
	defer whisperSrv.Close()

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeDummyAudio(t, dir, "call.m4a", mtime)

	cfg := &config.Config{
		WhisperAudioDir:    dir,
		WhisperAPIURL:      whisperSrv.URL,
		WhisperModel:       "whisper-1",
		WhisperLanguage:    "ko",
		DiarizationEnabled: true,
		DiarizationAPIURL:  "http://8.8.8.8:9999", // public IP — must never be dialled
	}
	c := makeWhisperCollector(cfg, whisperSrv)
	// Bound the (should-never-happen) dial attempt so the test fails fast
	// rather than hanging if the guard is ever removed/bypassed.
	c.httpClient.Timeout = 2 * time.Second

	docs, err := c.Collect(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Collect() returned %d docs, want 1 (fallback)", len(docs))
	}
	if docs[0].Content != wantFallbackText {
		t.Errorf("Content = %q, want plain fallback %q (diarization must be refused for non-local endpoint)",
			docs[0].Content, wantFallbackText)
	}
	if _, ok := docs[0].Metadata["speaker_count"]; ok {
		t.Error("speaker_count must not be set when diarization endpoint is non-local")
	}
}
