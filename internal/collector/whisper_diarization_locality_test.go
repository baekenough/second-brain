package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	// diarizeAudio uses diarizeHTTPClient (NOT httpClient, which is
	// transcribeFile-only, #169) — override that client's transport.
	c.diarizeHTTPClient = &http.Client{Transport: rt}

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
//
// This deliberately does NOT override c.diarizeHTTPClient: srv.URL's host is
// the literal IP "127.0.0.1", so verifiedDialContext's "numeric IP literal"
// branch verifies and dials it directly using the collector's own
// production diarizeHTTPClient built by NewWhisperCollector — proving the new
// dial-time verifier works end-to-end for a real loopback connection, not
// just when the test swaps in srv.Client().
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

// TestDiarizeAudio_DialTimeRefusesSingleLabelHostResolvingToPublicIP is test
// (a) from issue #169: isLocalWhisperEndpoint's single-label-hostname branch
// (case 2 in its doc comment) trusts ANY dotless hostname as local with zero
// DNS resolution — a Docker/compose service name convention. This test proves
// that even though the pre-flight check therefore passes unconditionally,
// verifiedDialContext independently resolves the host at dial time via the
// injected resolver and refuses to dial when the resolved address is public.
func TestDiarizeAudio_DialTimeRefusesSingleLabelHostResolvingToPublicIP(t *testing.T) {
	t.Parallel()

	const host = "diarizehost" // single-label — no dot
	url := "http://" + host + ":9999"

	// Sanity: confirm the cheap pre-flight check trusts this host unconditionally
	// (the exact gap this dial-time verifier closes).
	if !isLocalWhisperEndpoint(url) {
		t.Fatalf("isLocalWhisperEndpoint(%q) = false, want true (single-label hostnames are trusted by the pre-flight check)", url)
	}

	cfg := &config.Config{
		WhisperModel:       "whisper-1",
		DiarizationEnabled: true,
		DiarizationAPIURL:  url,
	}
	c := NewWhisperCollector(cfg)
	c.diarizeResolveHost = func(_ context.Context, gotHost string) ([]string, error) {
		if gotHost != host {
			t.Errorf("diarizeResolveHost called with host = %q, want %q", gotHost, host)
		}
		return []string{"8.8.8.8"}, nil // public IP — simulates a hostile/misconfigured resolver
	}

	segs, err := c.diarizeAudio(context.Background(), "call.m4a", []byte("fake-audio-bytes"), false)
	if err == nil {
		t.Fatal("diarizeAudio() error = nil, want non-nil — dial-time verification must refuse a public resolved address (#169)")
	}
	if segs != nil {
		t.Errorf("diarizeAudio() segments = %v, want nil", segs)
	}
}

// TestDiarizeAudio_DialTimeAllowsResolvedLoopbackHost is test (b) from issue
// #169: it exercises verifiedDialContext's "resolve, then dial the pinned
// result" branch (as opposed to TestDiarizeAudio_AllowsLocalEndpoint, which
// exercises the "already a numeric IP literal" branch) end-to-end against a
// real httptest.Server, proving the resolve path still works for a
// legitimately local hostname.
func TestDiarizeAudio_DialTimeAllowsResolvedLoopbackHost(t *testing.T) {
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

	// srv.URL is "http://127.0.0.1:<port>" — swap the host for a hostname so
	// the dial goes through the resolve branch instead of the IP-literal
	// branch, while diarizeResolveHost pins that hostname back to the
	// server's real loopback address.
	srvURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse srv.URL: %v", err)
	}
	const host = "diarize-loopback-alias"
	hostnameURL := "http://" + host + ":" + srvURL.Port()

	cfg := &config.Config{
		WhisperModel:       "whisper-1",
		DiarizationEnabled: true,
		DiarizationAPIURL:  hostnameURL,
	}
	c := NewWhisperCollector(cfg)
	c.diarizeResolveHost = func(_ context.Context, gotHost string) ([]string, error) {
		if gotHost != host {
			t.Errorf("diarizeResolveHost called with host = %q, want %q", gotHost, host)
		}
		return []string{"127.0.0.1"}, nil
	}

	segs, err := c.diarizeAudio(context.Background(), "call.m4a", []byte("fake-audio-bytes"), false)
	if err != nil {
		t.Fatalf("diarizeAudio() error = %v, want nil for a hostname resolving to loopback", err)
	}
	if len(segs) != 1 {
		t.Errorf("diarizeAudio() segments = %v, want 1 segment", segs)
	}
	if !called {
		t.Error("diarization server was never called for a hostname resolving to loopback")
	}
}

// TestDiarizeAudio_DialTimeRefusesRebindingEvenWhenPreflightPassed is test (c)
// from issue #169 (DNS rebinding / check-dial TOCTOU): "localhost" is one of
// isLocalWhisperEndpoint's well-known literal aliases (case 1), so the
// pre-flight check passes WITHOUT ever resolving the host. This test proves
// that verifiedDialContext still independently resolves "localhost" at dial
// time and refuses the connection when that resolution (here, simulated via
// the injected resolver) returns a public address — i.e. the dial-time
// defence holds even for the pre-flight check's most-trusted, no-DNS-at-all
// fast path.
func TestDiarizeAudio_DialTimeRefusesRebindingEvenWhenPreflightPassed(t *testing.T) {
	t.Parallel()

	const url = "http://localhost:9999"

	// Sanity: confirm the pre-flight check passes via the well-known-alias
	// fast path (no DNS lookup performed at all).
	if !isLocalWhisperEndpoint(url) {
		t.Fatalf("isLocalWhisperEndpoint(%q) = false, want true ('localhost' is a well-known alias)", url)
	}

	cfg := &config.Config{
		WhisperModel:       "whisper-1",
		DiarizationEnabled: true,
		DiarizationAPIURL:  url,
	}
	c := NewWhisperCollector(cfg)
	c.diarizeResolveHost = func(_ context.Context, gotHost string) ([]string, error) {
		if gotHost != "localhost" {
			t.Errorf("diarizeResolveHost called with host = %q, want %q", gotHost, "localhost")
		}
		// Simulates a rebinding attacker (or plain misconfiguration) answering
		// the dial-time lookup with a public address, even though the
		// pre-flight string check above already "passed".
		return []string{"8.8.8.8"}, nil
	}

	segs, err := c.diarizeAudio(context.Background(), "call.m4a", []byte("fake-audio-bytes"), false)
	if err == nil {
		t.Fatal("diarizeAudio() error = nil, want non-nil — dial-time verification must refuse a rebound public address even though the pre-flight check passed (#169)")
	}
	if segs != nil {
		t.Errorf("diarizeAudio() segments = %v, want nil", segs)
	}
}

// TestDiarizeAudio_RefusesRedirect verifies that when a "local" diarization
// endpoint (passing isLocalWhisperEndpoint) responds with an HTTP redirect,
// diarizeHTTPClient's CheckRedirect guard refuses to follow it — so the
// redirect target is NEVER dialled, even though it points at a different
// (potentially non-local) host. This closes the gap where a locally-approved
// endpoint could redirect audio bytes off-box after the locality check has
// already passed (issue #165 follow-up).
//
// The collector under test is constructed via NewWhisperCollector and its
// diarizeHTTPClient is deliberately left untouched (unlike makeWhisperCollector,
// which overrides httpClient — the transcribe-only client — with srv.Client()
// and has no effect on diarizeHTTPClient).
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
	c.diarizeHTTPClient.Timeout = 5 * time.Second // bound the test if the guard regresses

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
	// The public DiarizationAPIURL is refused by the isLocalWhisperEndpoint
	// pre-flight check in diarizeAudio before any client (transcribe or
	// diarize) ever dials it, so no timeout override is needed here to bound
	// a dial attempt. Bound diarizeHTTPClient anyway as defence-in-depth in
	// case the pre-flight check ever regresses.
	c.diarizeHTTPClient.Timeout = 2 * time.Second

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
