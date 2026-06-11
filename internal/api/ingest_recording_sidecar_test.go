package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestIngestRecording_SidecarWrittenForCall verifies that a successful call
// recording upload writes a {audioFile}.meta.json sidecar alongside the audio
// file containing the expected recording metadata fields.
func TestIngestRecording_SidecarWrittenForCall(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, recordingDir := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	number := "01012345678"
	dateMs := int64(1705311000000) // 2024-01-15 09:30:00 UTC
	contactName := "Alice"

	body, ct := buildRecordingForm(t, "call.m4a", validM4ABytes(32), number, dateMs,
		"duration_sec", "120",
		"contact_name", contactName,
	)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")
	if rr.Code != 201 {
		t.Fatalf("status = %d, want 201; body: %s", rr.Code, rr.Body.String())
	}

	// Find the audio file and its sidecar.
	entries, err := os.ReadDir(recordingDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	// Expect exactly one audio file + one sidecar = 2 entries.
	if len(entries) != 2 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("expected 2 files (audio + sidecar) in recordingDir, got %d: %v", len(entries), names)
	}

	// Identify audio file and sidecar.
	var audioFile, sidecarFile string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			sidecarFile = e.Name()
		} else {
			audioFile = e.Name()
		}
	}
	if audioFile == "" {
		t.Fatal("audio file not found in recordingDir")
	}
	if sidecarFile == "" {
		t.Fatal("sidecar .meta.json not found in recordingDir")
	}

	// Sidecar must be named {audioFile}.meta.json.
	wantSidecar := audioFile + ".meta.json"
	if sidecarFile != wantSidecar {
		t.Errorf("sidecar file = %q, want %q", sidecarFile, wantSidecar)
	}

	// Parse and validate sidecar contents.
	sidecarData, err := os.ReadFile(filepath.Join(recordingDir, sidecarFile))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}

	var sidecar map[string]any
	if err := json.Unmarshal(sidecarData, &sidecar); err != nil {
		t.Fatalf("unmarshal sidecar: %v", err)
	}

	if sidecar["contact_name"] != contactName {
		t.Errorf("sidecar contact_name = %v, want %q", sidecar["contact_name"], contactName)
	}
	if sidecar["direction"] != "incoming" {
		t.Errorf("sidecar direction = %v, want incoming", sidecar["direction"])
	}
	if sidecar["recording_type"] != "call" {
		t.Errorf("sidecar recording_type = %v, want call", sidecar["recording_type"])
	}
	// JSON numbers unmarshal as float64.
	durF, _ := sidecar["duration_seconds"].(float64)
	if int(durF) != 120 {
		t.Errorf("sidecar duration_seconds = %v, want 120", sidecar["duration_seconds"])
	}
}

// TestIngestRecording_SidecarWrittenForVoiceMemo verifies that a voice-memo
// upload writes a sidecar with recording_type="voice-memo" and no direction field.
func TestIngestRecording_SidecarWrittenForVoiceMemo(t *testing.T) {
	t.Parallel()

	upserter := &stubIngestUpserter{}
	srv, recordingDir := newRecordingTestServer(t, upserter, "", 0, time.Time{})

	dateMs := time.Now().Add(-time.Hour).UnixMilli()

	body, ct := buildRecordingForm(t, "memo.m4a", validM4ABytes(32), "" /* no number */, dateMs,
		"kind", "voice-memo",
		"duration_sec", "45",
		"contact_name", "회의 메모",
	)
	rr := doRecordingPost(t, srv, body, ct, "Bearer test-key")
	if rr.Code != 201 {
		t.Fatalf("status = %d, want 201; body: %s", rr.Code, rr.Body.String())
	}

	// Locate the sidecar.
	entries, _ := os.ReadDir(recordingDir)
	var sidecarPath string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			sidecarPath = filepath.Join(recordingDir, e.Name())
		}
	}
	if sidecarPath == "" {
		t.Fatal("sidecar .meta.json not found in recordingDir")
	}

	sidecarData, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}

	var sidecar map[string]any
	if err := json.Unmarshal(sidecarData, &sidecar); err != nil {
		t.Fatalf("unmarshal sidecar: %v", err)
	}

	if sidecar["recording_type"] != "voice-memo" {
		t.Errorf("sidecar recording_type = %v, want voice-memo", sidecar["recording_type"])
	}
	if _, ok := sidecar["direction"]; ok {
		t.Errorf("sidecar direction should be absent for voice-memo, got %v", sidecar["direction"])
	}
	if sidecar["contact_name"] != "회의 메모" {
		t.Errorf("sidecar contact_name = %v, want 회의 메모", sidecar["contact_name"])
	}
}

// TestIngestRecording_SidecarMarshalling verifies the recordingSidecar struct
// marshals as expected JSON — a unit test that does not touch HTTP or disk.
func TestIngestRecording_SidecarMarshalling(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		sidecar  recordingSidecar
		wantKeys map[string]any
		noKeys   []string
	}{
		{
			name: "call with contact",
			sidecar: recordingSidecar{
				ContactName:     "Bob",
				Number:          "01099998888",
				Direction:       "incoming",
				RecordingType:   "call",
				DurationSeconds: 90,
				DateMs:          1705311000000,
				Kind:            "call",
			},
			wantKeys: map[string]any{
				"contact_name":     "Bob",
				"number":           "01099998888",
				"direction":        "incoming",
				"recording_type":   "call",
				"duration_seconds": float64(90),
				"date_ms":          float64(1705311000000),
				"kind":             "call",
			},
		},
		{
			name: "voice-memo omits direction",
			sidecar: recordingSidecar{
				ContactName:     "내 메모",
				RecordingType:   "voice-memo",
				DurationSeconds: 30,
				Kind:            "voice-memo",
			},
			wantKeys: map[string]any{
				"contact_name":     "내 메모",
				"recording_type":   "voice-memo",
				"duration_seconds": float64(30),
				"kind":             "voice-memo",
			},
			noKeys: []string{"direction", "number"},
		},
		{
			name: "anonymous call omits contact_name and number",
			sidecar: recordingSidecar{
				Direction:       "incoming",
				RecordingType:   "call",
				DurationSeconds: 60,
				Kind:            "call",
			},
			wantKeys: map[string]any{
				"direction":        "incoming",
				"recording_type":   "call",
				"duration_seconds": float64(60),
			},
			noKeys: []string{"contact_name", "number"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(tc.sidecar)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}

			var got map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}

			for key, wantVal := range tc.wantKeys {
				gotVal, ok := got[key]
				if !ok {
					t.Errorf("key %q missing from JSON output", key)
					continue
				}
				if fmt.Sprintf("%v", gotVal) != fmt.Sprintf("%v", wantVal) {
					t.Errorf("key %q = %v (%T), want %v (%T)", key, gotVal, gotVal, wantVal, wantVal)
				}
			}

			for _, key := range tc.noKeys {
				if val, ok := got[key]; ok {
					t.Errorf("key %q should be absent (omitempty), got %v", key, val)
				}
			}
		})
	}
}
